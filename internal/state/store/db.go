package store

import (
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
		rawDB, err = sql.Open("sqlite", dbPath+"?_journal_mode=WAL")
		if err != nil {
			return nil, fmt.Errorf("state store: open db: %w", err)
		}
		if _, err = rawDB.Exec("PRAGMA busy_timeout = 5000"); err != nil {
			_ = rawDB.Close()
			return nil, fmt.Errorf("state store: busy_timeout: %w", err)
		}
		if _, err = rawDB.Exec("PRAGMA journal_mode=WAL"); err != nil {
			_ = rawDB.Close()
			return nil, fmt.Errorf("state store: WAL: %w", err)
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
	// Ensure schema_version exists (idempotent).
	if _, err := d.db.Exec("CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL PRIMARY KEY)"); err != nil {
		return fmt.Errorf("migrations: create schema_version: %w", err)
	}
	current, err := d.currentVersion()
	if err != nil {
		return err
	}
	names, err := migrationNames()
	if err != nil {
		return err
	}
	for _, name := range names {
		n, err := migrationNumber(name)
		if err != nil || n <= 0 {
			continue
		}
		if n <= current {
			continue
		}
		sqlStr, err := migrationSQL(name)
		if err != nil {
			return fmt.Errorf("migration %s: %w", name, err)
		}
		tx, err := d.db.Begin()
		if err != nil {
			return fmt.Errorf("migration %s: begin: %w", name, err)
		}
		if _, err := tx.Exec(sqlStr); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err := tx.Exec("DELETE FROM schema_version"); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %s: clear version: %w", name, err)
		}
		if _, err := tx.Exec(d.dialect.Rebind("INSERT INTO schema_version (version) VALUES (?)"), n); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %s: set version: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migration %s: commit: %w", name, err)
		}
	}
	return nil
}

func (d *DB) currentVersion() (int, error) {
	var v sql.NullInt64
	err := d.db.QueryRow("SELECT version FROM schema_version LIMIT 1").Scan(&v)
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
