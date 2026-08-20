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

// Run prunes once immediately and then on each tick until ctx is cancelled,
// returning ctx.Err().
//
// The immediate pass at the top matters because an agent restarted more often
// than the interval — a redeploy, a host reboot, a crash loop — would
// otherwise never enforce retention at all: a ticker that only fires after
// defaultPruneInterval never gets there before the process is replaced again.
// It runs synchronously before the loop rather than on a shortened first
// delay because runAll (cmd/devmon-agent/main.go) already starts this
// alongside the HTTP listener as an independent goroutine, so this pass
// overlaps with — and never blocks — the agent coming up. If ctx is already
// cancelled on entry, Run returns immediately without touching the store.
func (p *Pruner) Run(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.pruneOnce(ctx)

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

// pruneOnce enforces both retention policies. Each prune is attempted
// independently — a failure in one must not skip the other, since audit
// retention and pairing-code retention protect different things (disk budget
// vs. an unbounded pairing_codes table) and neither's failure implies
// anything about the other's health.
func (p *Pruner) pruneOnce(ctx context.Context) {
	p.pruneAudit(ctx)
	p.prunePairingCodes(ctx)
}

func (p *Pruner) pruneAudit(ctx context.Context) {
	removed, err := p.store.PruneAudit(ctx, p.maxAge, p.maxRows)
	if err != nil {
		p.log.Error("audit prune failed", slog.Any("err", err))
		return
	}
	if removed > 0 {
		p.log.Info("audit rows pruned", slog.Int64("removed", removed))
	}
}

// prunePairingCodes removes expired pairing codes so a minted code — used,
// unused, or expired — does not stay in the table forever. Only the count is
// ever logged: pairing codes themselves must never appear in logs, at any
// level.
func (p *Pruner) prunePairingCodes(ctx context.Context) {
	removed, err := p.store.PrunePairingCodes(ctx)
	if err != nil {
		p.log.Error("pairing code prune failed", slog.Any("err", err))
		return
	}
	if removed > 0 {
		p.log.Info("pairing codes pruned", slog.Int64("removed", removed))
	}
}
