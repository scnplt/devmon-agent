// SPDX-License-Identifier: AGPL-3.0-only

package logging

import (
	"context"
	"log/slog"
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

// Run rotates on each tick until ctx is cancelled, returning ctx.Err().
//
// A failed rotation is logged and the loop continues: losing retention is bad,
// but taking the agent down over it is worse.
func (r *Rotator) Run(ctx context.Context) error {
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
