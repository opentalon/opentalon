package store

import (
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Open opens the SQLite database at dataDir/state.db, creating dataDir if needed.
// It enables WAL mode and runs pending migrations. Caller must call Close when done.
func Open(dataDir string) (*DB, error) {
	if dataDir == "" {
		return nil, fmt.Errorf("state store: data_dir is required")
	}
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, fmt.Errorf("state store: %w", err)
	}
	dbPath := filepath.Join(dataDir, "state.db")
	db, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL")
	if err != nil {
		return nil, fmt.Errorf("state store: open db: %w", err)
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("state store: WAL: %w", err)
	}
	d := &DB{db: db}
	if err := d.runMigrations(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return d, nil
}

// DB holds the SQLite connection and runs migrations on Open.
type DB struct {
	db *sql.DB
}

// DB returns the underlying *sql.DB for use by stores. Do not close it directly; use Close on DB.
func (d *DB) SQLDB() *sql.DB {
	return d.db
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
		sql, err := migrationSQL(name)
		if err != nil {
			return fmt.Errorf("migration %s: %w", name, err)
		}
		tx, err := d.db.Begin()
		if err != nil {
			return fmt.Errorf("migration %s: begin: %w", name, err)
		}
		if _, err := tx.Exec(sql); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err := tx.Exec("DELETE FROM schema_version"); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %s: clear version: %w", name, err)
		}
		if _, err := tx.Exec("INSERT INTO schema_version (version) VALUES (?)", n); err != nil {
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
