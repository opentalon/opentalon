package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	_ "modernc.org/sqlite"
)

// RunPluginMigrations opens or creates the plugin DB at dataDir/plugin_data/pluginName.db
// and runs migrations from pluginPath/migrations/*.sql in order. Same runner as
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
	// See db.go: modernc pragma syntax, applied per-connection via the DSN.
	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return fmt.Errorf("plugin db open: %w", err)
	}
	defer func() { _ = db.Close() }()
	if err := verifySQLitePragmas(db); err != nil {
		return fmt.Errorf("plugin db: %w", err)
	}
	if err := migrate(db, SQLiteDialect, names, func(name string) (string, error) {
		data, err := os.ReadFile(filepath.Join(migrationsDir, name))
		return string(data), err
	}); err != nil {
		return fmt.Errorf("plugin %w", err)
	}
	return nil
}
