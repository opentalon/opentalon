//go:build postgres

package channel

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/opentalon/opentalon/internal/config"
	"github.com/opentalon/opentalon/internal/state/store"
)

// Run with: DATABASE_URL='postgres://localhost/opentalon_test?sslmode=disable' go test -tags postgres -run '/postgres' ./internal/channel/

func init() { storeKinds = append(storeKinds, storeKind{"postgres", postgresStore}) }

// postgresStore migrates a schema of its own, so packages whose Postgres tests run at once never share tables.
func postgresStore(t *testing.T) testStore {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set")
	}
	suffix := make([]byte, 8)
	_, _ = rand.Read(suffix)
	schema := "test_" + hex.EncodeToString(suffix)
	admin, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) })
	db, err := store.Open(config.DBConfig{Driver: "postgres", DSN: withSearchPath(t, dsn, schema)}, "")
	if err != nil {
		t.Fatalf("open postgres store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return dbStore(db)
}

func withSearchPath(t *testing.T, dsn, schema string) string {
	t.Helper()
	if !strings.Contains(dsn, "://") {
		return dsn + " search_path=" + schema
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("DATABASE_URL: %v", err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}
