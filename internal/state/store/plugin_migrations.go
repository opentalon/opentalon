package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"
)

// RunPluginMigrations opens or creates the plugin DB at dataDir/plugin_data/pluginName.db
// and runs migrations from pluginPath/migrations/*.sql in order. Same versioned pattern as
// the main DB (schema_version table). If pluginPath/migrations does not exist, no-op.
func RunPluginMigrations(dataDir, pluginName, pluginPath string) error {
	if dataDir == "" || pluginName == "" || pluginPath == "" {
		return nil
	}
	migrationsDir := filepath.Join(pluginPath, "migrations")
	entries, err := os.ReadDir(migrationsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("plugin migrations read dir: %w", err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		names = append(names, e.Name())
	}
	if len(names) == 0 {
		return nil
	}
	sort.Strings(names)

	pluginDataDir := filepath.Join(dataDir, "plugin_data")
	if err := os.MkdirAll(pluginDataDir, 0700); err != nil {
		return fmt.Errorf("plugin_data dir: %w", err)
	}
	dbPath := filepath.Join(pluginDataDir, pluginName+".db")
	db, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL")
	if err != nil {
		return fmt.Errorf("plugin db open: %w", err)
	}
	defer db.Close()

	if _, err := db.Exec("CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL PRIMARY KEY)"); err != nil {
		return fmt.Errorf("plugin schema_version: %w", err)
	}
	var current int
	var v sql.NullInt64
	if err := db.QueryRow("SELECT version FROM schema_version LIMIT 1").Scan(&v); err == nil && v.Valid {
		current = int(v.Int64)
	}

	for _, name := range names {
		n, err := pluginMigrationNumber(name)
		if err != nil || n <= 0 {
			continue
		}
		if n <= current {
			continue
		}
		sqlPath := filepath.Join(migrationsDir, name)
		data, err := os.ReadFile(sqlPath)
		if err != nil {
			return fmt.Errorf("plugin migration %s: %w", name, err)
		}
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("plugin migration %s begin: %w", name, err)
		}
		if _, err := tx.Exec(string(data)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("plugin migration %s: %w", name, err)
		}
		if _, err := tx.Exec("DELETE FROM schema_version"); err != nil {
			_ = tx.Rollback()
			return err
		}
		if _, err := tx.Exec("INSERT INTO schema_version (version) VALUES (?)", n); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func pluginMigrationNumber(name string) (int, error) {
	base := strings.TrimSuffix(name, ".sql")
	parts := strings.SplitN(base, "_", 2)
	if len(parts) < 2 {
		return 0, fmt.Errorf("invalid name")
	}
	return strconv.Atoi(parts[0])
}
