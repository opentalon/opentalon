package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/opentalon/opentalon/internal/config"
	"github.com/opentalon/opentalon/internal/state"
	"github.com/opentalon/opentalon/internal/state/store"
)

type noteSessions interface {
	SessionStoreInterface
	ReserveNote(ctx context.Context, sessionID, noteKey string, now time.Time, staleAfter time.Duration) (state.ClaimResult, error)
}

type storeKind struct {
	name string
	open func(t *testing.T) noteSessions
}

// storeKinds gains the PostgreSQL store under the postgres build tag (postgres_test.go).
var storeKinds = []storeKind{
	{"memory", func(*testing.T) noteSessions { return state.NewSessionStore("") }},
	{"sqlite", func(t *testing.T) noteSessions {
		t.Helper()
		db, err := store.Open(config.DBConfig{}, t.TempDir())
		if err != nil {
			t.Fatalf("open sqlite: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return store.NewSessionStore(db, 0, 0)
	}},
}

func eachStore(t *testing.T, test func(t *testing.T, kind storeKind)) {
	t.Helper()
	for _, kind := range storeKinds {
		t.Run(kind.name, func(t *testing.T) { test(t, kind) })
	}
}
