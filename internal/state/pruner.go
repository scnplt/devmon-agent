// SPDX-License-Identifier: AGPL-3.0-only

package state

import (
	"context"
	"log/slog"
	"time"
)

// defaultPruneInterval is how often audit retention is enforced. Six hours is
// frequent enough that an over-budget table is corrected the same day and rare
// enough that the DELETE never competes with request traffic.
const defaultPruneInterval = 6 * time.Hour

// Pruner enforces audit retention on a fixed interval.
//
// Phase 1 writes no audit rows, so this is a no-op against an empty table. It
// ships now anyway: retention configuration, the schema, and its enforcement are
// one story, and validating them together beats splitting them across phases.
type Pruner struct {
	store    *Store
	maxAge   time.Duration
	maxRows  int
	interval time.Duration
	log      *slog.Logger
}

// NewPruner returns a Pruner applying the given retention bounds.
func NewPruner(store *Store, maxAge time.Duration, maxRows int, log *slog.Logger) *Pruner {
	return &Pruner{
		store:    store,
		maxAge:   maxAge,
		maxRows:  maxRows,
		interval: defaultPruneInterval,
		log:      log,
	}
}

// Run prunes on each tick until ctx is cancelled, returning ctx.Err().
//
// A failed prune is logged and the loop continues: an over-budget audit table is
// a disk problem, not a reason to stop serving.
func (p *Pruner) Run(ctx context.Context) error {
	t := time.NewTicker(p.interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			p.pruneOnce(ctx)
		}
	}
}

func (p *Pruner) pruneOnce(ctx context.Context) {
	removed, err := p.store.PruneAudit(ctx, p.maxAge, p.maxRows)
	if err != nil {
		p.log.Error("audit prune failed", slog.Any("err", err))
		return
	}
	if removed > 0 {
		p.log.Info("audit rows pruned", slog.Int64("removed", removed))
	}
}
