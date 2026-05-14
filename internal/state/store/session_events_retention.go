package store

import (
	"context"
	"log/slog"
	"time"
)

// RunSessionEventsRetention prunes session_events rows older than
// retention on a daily schedule. Runs an immediate prune on start so a
// freshly-rotated pod catches up rather than waiting up to 24h.
//
// retention <= 0 disables pruning entirely — the caller (main.go) maps
// the explicit SessionEventsConfig.RetentionDisabled flag into this.
// Treating zero as "disabled" here is intentional API simplicity;
// callers either pass a positive duration or pass zero. The "0 days
// means default" UX trick stays at the config layer.
//
// Implementation deliberately mirrors RunDebugRetention: same in-process
// timer, same DELETE-by-timestamp, no pg_cron / pg_partman dependency
// (SQLite deployments would otherwise diverge from production Postgres).
func RunSessionEventsRetention(ctx context.Context, store *SessionEventStore, retention time.Duration) {
	if retention <= 0 {
		slog.Info("session events retention disabled")
		return
	}
	prune := func() {
		pruneCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		n, err := store.Prune(pruneCtx, retention)
		if err != nil {
			slog.Warn("session events retention prune failed", "error", err)
			return
		}
		if n > 0 {
			slog.Info("session events retention prune", "rows_deleted", n, "retention", retention)
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
