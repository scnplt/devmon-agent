// SPDX-License-Identifier: AGPL-3.0-only

package logging

import (
	"context"
	"log/slog"
	"os"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"
)

// defaultRotateInterval is one day, matching the granularity of
// DEVMON_LOG_MAX_AGE_DAYS.
const defaultRotateInterval = 24 * time.Hour

// Rotator forces a log rotation on a fixed interval.
//
// This is NOT redundant with lumberjack's own behaviour, and deleting it would
// silently break age-based retention. lumberjack rotates on SIZE only, and it
// applies MaxAge only at the moment a rotation happens. A quiet agent never
// fills a file, so it never rotates, so it never prunes — its agent.log would
// keep entries from months ago while sitting comfortably under the size cap.
// The ticker is what makes DEVMON_LOG_MAX_AGE_DAYS mean anything.
type Rotator struct {
	lj       *lumberjack.Logger
	interval time.Duration
	log      *slog.Logger
}

// NewRotator returns a Rotator that rotates lj every interval.
func NewRotator(lj *lumberjack.Logger, interval time.Duration, log *slog.Logger) *Rotator {
	return &Rotator{lj: lj, interval: interval, log: log}
}

// Run rotates once immediately and then on each tick until ctx is cancelled,
// returning ctx.Err().
//
// The immediate pass matters for the same reason the Rotator exists at all:
// lumberjack only applies MaxAge at the moment a rotation happens, and an
// agent restarted more often than r.interval (a redeploy, a host reboot, a
// crash loop) would otherwise never rotate — and so never enforce
// DEVMON_LOG_MAX_AGE_DAYS — before being replaced again. It runs
// synchronously before the loop rather than on a shortened first delay
// because runAll (cmd/devmon-agent/main.go) already starts this alongside
// the HTTP listener as an independent goroutine, so it overlaps with, and
// never blocks, the agent coming up. If ctx is already cancelled on entry,
// Run returns immediately without touching the log file.
//
// The startup pass calls startupRotate, not rotateOnce directly, to avoid
// spraying empty rotated backups on a crash-looping agent: verified against
// lumberjack v2.2.1's Logger.openNew, Rotate renames the current file aside
// whenever os.Stat on it succeeds, with no check on size — a zero-byte
// current file would still be renamed into a useless backup on every boot.
// A missing current file is harmless on its own (openNew just creates it,
// no rename), but skipping both missing and empty files here keeps a
// crash-looping agent's logs directory from accumulating backups no tick
// would ever have produced.
//
// A failed rotation is logged and the loop continues: losing retention is bad,
// but taking the agent down over it is worse.
func (r *Rotator) Run(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.startupRotate()

	t := time.NewTicker(r.interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			r.rotateOnce()
		}
	}
}

// startupRotate performs the startup rotation pass, skipping it when the
// current log file is missing or empty — see the rationale in Run's doc
// comment.
func (r *Rotator) startupRotate() {
	info, err := os.Stat(r.lj.Filename)
	if err != nil || info.Size() == 0 {
		return
	}
	r.rotateOnce()
}

func (r *Rotator) rotateOnce() {
	if err := r.lj.Rotate(); err != nil {
		r.log.Error("log rotation failed", slog.Any("err", err))
		return
	}
	// lumberjack creates the fresh file with 0600 already, but it re-creates it
	// on every rotation; re-asserting the mode keeps the guarantee independent
	// of that implementation detail.
	if err := tightenPermissions(r.lj.Filename); err != nil {
		r.log.Warn("could not tighten log file permissions", slog.Any("err", err))
	}
}
