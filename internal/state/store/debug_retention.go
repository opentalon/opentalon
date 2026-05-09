package store

import (
	"context"
	"log/slog"
	"time"
)

// RunDebugRetention prunes ai_debug_events rows older than retention on a
// daily schedule. Runs an immediate prune on start so a freshly-rotated pod
// catches up rather than waiting up to 24h.
//
// retention <= 0 disables pruning entirely — the caller (main.go) maps the
// explicit DebugConfig.RetentionDisabled flag into this. Treating zero as
// "disabled" here is intentional API simplicity: callers either pass a
// positive duration or pass zero. The "0 days means 30 days" UX trick stays
// at the config layer where users meet it.
//
// We don't use pg_cron or pg_partman: the retention policy is a single
// DELETE-by-timestamp, doable in milliseconds at the volumes this table is
// designed for, and keeping the logic in process means SQLite deployments
// (dev / single-node) get the same behavior as production Postgres without
// extension dependencies.
//
// Cancellation: returns when ctx is cancelled. Stop() on the writer is a
// separate concern handled in main.go.
func RunDebugRetention(ctx context.Context, store *DebugEventStore, retention time.Duration) {
	if retention <= 0 {
		slog.Info("debug retention disabled")
		return
	}
	prune := func() {
		pruneCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		n, err := store.Prune(pruneCtx, retention)
		if err != nil {
			slog.Warn("debug retention prune failed", "error", err)
			return
		}
		if n > 0 {
			slog.Info("debug retention prune", "rows_deleted", n, "retention", retention)
		}
	}

	prune()
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			prune()
		}
	}
}
