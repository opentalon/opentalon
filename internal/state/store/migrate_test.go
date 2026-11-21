package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/opentalon/opentalon/internal/config"
)

func latestMigration(t *testing.T) int {
	t.Helper()
	names, err := migrationNames()
	if err != nil {
		t.Fatalf("migration names: %v", err)
	}
	n, err := migrationNumber(names[len(names)-1])
	if err != nil {
		t.Fatalf("migration number: %v", err)
	}
	return n
}

func setSchemaBackTo20(t *testing.T, db *DB) {
	t.Helper()
	for _, q := range []string{
		`ALTER TABLE messages DROP COLUMN announcement`,
		`ALTER TABLE messages DROP COLUMN message_key`,
		`DELETE FROM schema_version`,
		`INSERT INTO schema_version (version) VALUES (20)`,
	} {
		if _, err := db.SQLDB().Exec(q); err != nil {
			t.Fatalf("set the schema back to 20: %s: %v", q, err)
		}
	}
}

func runConcurrently(n int, run func(i int)) {
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			run(i)
		}()
	}
	close(start)
	wg.Wait()
}

func checkConcurrentOpens(t *testing.T, open func() (*DB, error)) {
	t.Helper()
	const opens = 8
	dbs := make([]*DB, opens)
	errs := make([]error, opens)
	runConcurrently(opens, func(i int) { dbs[i], errs[i] = open() })
	var opened *DB
	for i, db := range dbs {
		if errs[i] != nil {
			t.Errorf("Open %d of %d: %v", i+1, opens, errs[i])
			continue
		}
		t.Cleanup(func() { _ = db.Close() })
		opened = db
	}
	if opened == nil {
		t.FailNow()
	}
	var rows, version int
	if err := opened.SQLDB().QueryRow(`SELECT COUNT(*), MAX(version) FROM schema_version`).Scan(&rows, &version); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if want := latestMigration(t); rows != 1 || version != want {
		t.Errorf("schema_version = %d rows, newest %d; want one row at %d", rows, version, want)
	}
	columns, err := opened.SQLDB().Query(`SELECT announcement, message_key FROM messages WHERE 1 = 0`)
	if err != nil {
		t.Fatalf("columns of migration 021: %v", err)
	}
	_ = columns.Close()
}

func TestOpen_ConcurrentOpensMigrateOnce(t *testing.T) {
	open := func(dir string) func() (*DB, error) {
		return func() (*DB, error) { return Open(config.DBConfig{}, dir) }
	}
	t.Run("from version 20", func(t *testing.T) {
		dir := t.TempDir()
		db, err := open(dir)()
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		setSchemaBackTo20(t, db)
		_ = db.Close()
		checkConcurrentOpens(t, open(dir))
	})
	t.Run("from an empty database", func(t *testing.T) {
		dir := t.TempDir()
		createEmptySQLiteFile(t, filepath.Join(dir, "state.db"))
		checkConcurrentOpens(t, open(dir))
	})
}

func createEmptySQLiteFile(t *testing.T, path string) {
	t.Helper()
	// Handles that create one new file at the same moment can fail in the WAL switch; that is not the migration runner.
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer func() { _ = db.Close() }()
	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode = WAL`).Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("create %s: journal_mode %q, %v", path, mode, err)
	}
}

func TestRunPluginMigrations_ConcurrentRunsApplyEachOnce(t *testing.T) {
	pluginDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(pluginDir, "migrations"), 0700); err != nil {
		t.Fatal(err)
	}
	const migrations = 20
	var want []int
	write := func(n int) {
		body := fmt.Sprintf(`INSERT INTO applied (n) VALUES (%d);`, n)
		if n == 1 {
			body = `CREATE TABLE applied (n INTEGER NOT NULL); ` + body
		}
		if err := os.WriteFile(filepath.Join(pluginDir, "migrations", fmt.Sprintf("%03d_step.sql", n)), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		want = append(want, n)
	}
	write(1)
	dataDir := t.TempDir()
	if err := RunPluginMigrations(dataDir, "plugin", pluginDir); err != nil {
		t.Fatalf("first run: %v", err)
	}
	for n := 2; n <= migrations; n++ {
		write(n)
	}
	const runs = 8
	errs := make([]error, runs)
	runConcurrently(runs, func(i int) { errs[i] = RunPluginMigrations(dataDir, "plugin", pluginDir) })
	for i, err := range errs {
		if err != nil {
			t.Errorf("run %d of %d: %v", i+1, runs, err)
		}
	}

	db, err := sql.Open("sqlite", filepath.Join(dataDir, "plugin_data", "plugin.db"))
	if err != nil {
		t.Fatalf("open the plugin database: %v", err)
	}
	defer func() { _ = db.Close() }()
	var applied []int
	rows, err := db.Query(`SELECT n FROM applied ORDER BY n`)
	if err != nil {
		t.Fatalf("read applied: %v", err)
	}
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan applied: %v", err)
		}
		applied = append(applied, n)
	}
	_ = rows.Close()
	if !slices.Equal(applied, want) {
		t.Errorf("applied migrations = %v, want each once: %v", applied, want)
	}
	var versions, version int
	if err := db.QueryRow(`SELECT COUNT(*), MAX(version) FROM schema_version`).Scan(&versions, &version); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if versions != 1 || version != migrations {
		t.Errorf("schema_version = %d rows, newest %d; want one row at %d", versions, version, migrations)
	}
}
