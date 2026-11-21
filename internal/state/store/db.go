package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "github.com/lib/pq"
	"github.com/opentalon/opentalon/internal/config"
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Open opens the database described by cfg.
// For sqlite (default): opens dataDir/state.db with WAL mode.
// For postgres: connects to cfg.DSN (dataDir is unused for the main connection but
// still needed for plugin databases).
// Caller must call Close when done.
func Open(cfg config.DBConfig, dataDir string) (*DB, error) {
	driver := cfg.Driver
	if driver == "" {
		driver = "sqlite"
	}

	var (
		rawDB   *sql.DB
		dialect Dialect
		err     error
	)

	switch driver {
	case "sqlite":
		dialect = SQLiteDialect
		if dataDir == "" {
			return nil, fmt.Errorf("state store: data_dir is required for sqlite")
		}
		if err = os.MkdirAll(dataDir, 0700); err != nil {
			return nil, fmt.Errorf("state store: %w", err)
		}
		dbPath := filepath.Join(dataDir, "state.db")
		// modernc.org/sqlite DSN pragma syntax is ?_pragma=name(value). These
		// apply on every connection the pool opens; busy_timeout is per-connection,
		// so it must live in the DSN, not a one-off Exec that only hits one conn.
		rawDB, err = sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
		if err != nil {
			return nil, fmt.Errorf("state store: open db: %w", err)
		}
		// Assert the pragmas actually applied — the original bug was a silently
		// ignored pragma, so verify rather than assume.
		if err = verifySQLitePragmas(rawDB); err != nil {
			_ = rawDB.Close()
			return nil, fmt.Errorf("state store: %w", err)
		}

	case "postgres":
		dialect = PostgresDialect
		if cfg.DSN == "" {
			return nil, fmt.Errorf("state store: dsn is required for postgres")
		}
		rawDB, err = sql.Open("postgres", cfg.DSN)
		if err != nil {
			return nil, fmt.Errorf("state store: open db: %w", err)
		}
		if err = rawDB.Ping(); err != nil {
			_ = rawDB.Close()
			return nil, fmt.Errorf("state store: ping postgres: %w", err)
		}
		// Connection pool tuning — sensible defaults for a system doing 10-15+
		// queries per request. Operators can override via config.
		maxOpen := cfg.MaxOpenConns
		if maxOpen <= 0 {
			maxOpen = 25
		}
		rawDB.SetMaxOpenConns(maxOpen)
		maxIdle := cfg.MaxIdleConns
		if maxIdle <= 0 {
			maxIdle = 10
		}
		rawDB.SetMaxIdleConns(maxIdle)
		if lt, err := time.ParseDuration(cfg.ConnMaxLifetime); err == nil && lt > 0 {
			rawDB.SetConnMaxLifetime(lt)
		} else {
			rawDB.SetConnMaxLifetime(5 * time.Minute)
		}
		if it, err := time.ParseDuration(cfg.ConnMaxIdleTime); err == nil && it > 0 {
			rawDB.SetConnMaxIdleTime(it)
		} else {
			rawDB.SetConnMaxIdleTime(5 * time.Minute)
		}

	default:
		return nil, fmt.Errorf("state store: unsupported driver %q (use \"sqlite\" or \"postgres\")", driver)
	}

	d := &DB{db: rawDB, dialect: dialect}
	prevVersion, _ := d.currentVersion()
	if err := d.runMigrations(); err != nil {
		_ = rawDB.Close()
		return nil, err
	}
	// Backfill entity_id/group_id on sessions created before migration 004.
	if prevVersion < 4 {
		d.backfillSessionOwnership()
	}
	return d, nil
}

// verifySQLitePragmas confirms WAL and busy_timeout took effect on a fresh
// connection. It fails loudly instead of silently running with default
// rollback-journal mode and no busy wait (the SQLITE_BUSY root cause).
func verifySQLitePragmas(db *sql.DB) error {
	var journal string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil {
		return fmt.Errorf("read journal_mode: %w", err)
	}
	if !strings.EqualFold(journal, "wal") {
		return fmt.Errorf("journal_mode is %q, want wal", journal)
	}
	var busy int
	if err := db.QueryRow("PRAGMA busy_timeout").Scan(&busy); err != nil {
		return fmt.Errorf("read busy_timeout: %w", err)
	}
	if busy != 5000 {
		return fmt.Errorf("busy_timeout is %d, want 5000", busy)
	}
	return nil
}

// DB holds the database connection and dialect.
type DB struct {
	db      *sql.DB
	dialect Dialect
}

// SQLDB returns the underlying *sql.DB. Do not close it directly; use Close on DB.
func (d *DB) SQLDB() *sql.DB {
	return d.db
}

// Dialect returns the dialect for this connection.
func (d *DB) Dialect() Dialect {
	return d.dialect
}

// Close closes the database connection.
func (d *DB) Close() error {
	return d.db.Close()
}

func (d *DB) runMigrations() error {
	names, err := migrationNames()
	if err != nil {
		return err
	}
	return migrate(d.db, d.dialect, names, migrationSQL)
}

func migrate(db *sql.DB, dialect Dialect, names []string, load func(name string) (string, error)) error {
	ctx := context.Background()
	if err := migrationStep(ctx, db, dialect, func(tx ExclusiveTx) error {
		_, err := tx.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL PRIMARY KEY)")
		return err
	}); err != nil {
		return fmt.Errorf("migrations: create schema_version: %w", err)
	}
	for _, name := range names {
		n, err := migrationNumber(name)
		if err != nil || n <= 0 {
			continue
		}
		if err := migrationStep(ctx, db, dialect, func(tx ExclusiveTx) error {
			current, err := readVersion(ctx, tx)
			if err != nil || n <= current {
				return err
			}
			sqlStr, err := load(name)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, sqlStr); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "DELETE FROM schema_version"); err != nil {
				return fmt.Errorf("clear version: %w", err)
			}
			if _, err := tx.ExecContext(ctx, dialect.Rebind("INSERT INTO schema_version (version) VALUES (?)"), n); err != nil {
				return fmt.Errorf("set version: %w", err)
			}
			return nil
		}); err != nil {
			return fmt.Errorf("migration %s: %w", name, err)
		}
	}
	return nil
}

func migrationStep(ctx context.Context, db *sql.DB, dialect Dialect, apply func(tx ExclusiveTx) error) error {
	tx, cleanup, err := dialect.beginMigration(ctx, db)
	if err != nil {
		return err
	}
	defer cleanup()
	if err := apply(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

func (d *DB) currentVersion() (int, error) {
	return readVersion(context.Background(), d.db)
}

type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func readVersion(ctx context.Context, q rowQuerier) (int, error) {
	var v sql.NullInt64
	err := q.QueryRowContext(ctx, "SELECT version FROM schema_version LIMIT 1").Scan(&v)
	if err == sql.ErrNoRows || (err == nil && !v.Valid) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("migrations: read version: %w", err)
	}
	return int(v.Int64), nil
}

func migrationNames() ([]string, error) {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func migrationNumber(name string) (int, error) {
	base := strings.TrimSuffix(name, ".sql")
	parts := strings.SplitN(base, "_", 2)
	if len(parts) < 2 {
		return 0, fmt.Errorf("invalid migration name")
	}
	n, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, err
	}
	return n, nil
}

func migrationSQL(name string) (string, error) {
	data, err := fs.ReadFile(migrationsFS, "migrations/"+name)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// backfillSessionOwnership populates entity_id and group_id on sessions
// created before migration 004 by parsing the session ID (entityID:channel:conv...).
func (d *DB) backfillSessionOwnership() {
	rows, err := d.db.Query(d.dialect.Rebind(`SELECT id FROM sessions WHERE entity_id = ''`))
	if err != nil {
		slog.Warn("session ownership backfill: query failed", "error", err)
		return
	}
	defer func() { _ = rows.Close() }()

	type update struct {
		id, entity string
	}
	var updates []update
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			slog.Warn("session ownership backfill: scan failed", "error", err)
			continue
		}
		parts := strings.SplitN(id, ":", 3)
		if len(parts) >= 3 {
			// Format: entityID:channelID:conversationID[...] — first segment is entity.
			updates = append(updates, update{id: id, entity: parts[0]})
		}
	}
	if err := rows.Err(); err != nil {
		slog.Warn("session ownership backfill: rows iteration failed", "error", err)
	}

	for _, u := range updates {
		// Look up group from entities table.
		var groupID string
		if err := d.db.QueryRow(d.dialect.Rebind(`SELECT COALESCE(group_id,'') FROM entities WHERE id = ?`), u.entity).Scan(&groupID); err != nil {
			slog.Warn("session ownership backfill: group lookup failed", "session", u.id, "entity", u.entity, "error", err)
		}
		if _, err := d.db.Exec(d.dialect.Rebind(`UPDATE sessions SET entity_id = ?, group_id = ? WHERE id = ?`), u.entity, groupID, u.id); err != nil {
			slog.Warn("session ownership backfill: update failed", "session", u.id, "error", err)
		}
	}
}
