package channel

import (
	"context"
	"testing"

	"github.com/opentalon/opentalon/internal/config"
	"github.com/opentalon/opentalon/internal/orchestrator"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
	"github.com/opentalon/opentalon/internal/state/store"
)

type sessionStore interface {
	SessionOpener
	orchestrator.SessionStoreInterface
	SetVerifiedOwner(ctx context.Context, id string, owner state.VerifiedOwner) error
}

// testStore can also write the owner fields a migration backfilled, which no store call writes.
type testStore struct {
	sessionStore
	backfill func(t *testing.T, id, entityID, groupID, channelID string)
	trim     func(t *testing.T, id string) // nil: the in-memory store keeps every row
}

type storeKind struct {
	name string
	open func(t *testing.T) testStore
}

// storeKinds gains the PostgreSQL store under the postgres build tag (postgres_test.go).
var storeKinds = []storeKind{
	{"memory", func(*testing.T) testStore { return memoryStore() }},
	{"sqlite", sqliteStore},
}

func eachStore(t *testing.T, test func(t *testing.T, kind storeKind)) {
	t.Helper()
	for _, kind := range storeKinds {
		t.Run(kind.name, func(t *testing.T) { test(t, kind) })
	}
}

func memoryStore() testStore {
	s := state.NewSessionStore("")
	return testStore{sessionStore: s, backfill: func(_ *testing.T, id, entityID, groupID, channelID string) {
		sess := s.Create(state.SessionParams{ID: id}) // the in-memory store's Create answers the live session
		sess.EntityID, sess.GroupID, sess.ChannelID = entityID, groupID, channelID
	}}
}

func sqliteStore(t *testing.T) testStore {
	t.Helper()
	db, err := store.Open(config.DBConfig{}, t.TempDir())
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return dbStore(db)
}

func dbStore(db *store.DB) testStore {
	return testStore{
		sessionStore: store.NewSessionStore(db, 0, 0),
		backfill: func(t *testing.T, id, entityID, groupID, channelID string) {
			t.Helper()
			if _, err := db.SQLDB().Exec(db.Dialect().Rebind(`UPDATE sessions SET entity_id = ?, group_id = ?, channel_id = ? WHERE id = ?`),
				entityID, groupID, channelID, id); err != nil {
				t.Fatalf("backfill %s: %v", id, err)
			}
		},
		trim: func(t *testing.T, id string) {
			t.Helper()
			if err := store.NewSessionStore(db, 1, 0).AddMessage(id, provider.Message{Role: provider.RoleUser, Content: "a later turn"}); err != nil {
				t.Fatalf("trim %s: %v", id, err)
			}
		},
	}
}
