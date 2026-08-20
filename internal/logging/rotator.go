// SPDX-License-Identifier: AGPL-3.0-only

package logging

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"
)

// defaultRotateInterval is one day, matching the granularity of
// DEVMON_LOG_MAX_AGE_DAYS.
const defaultRotateInterval = 24 * time.Hour

// slogTimeKeyPrefix is how slog's built-in TextHandler renders the record
// timestamp as the first key=value pair on every line, given the
// HandlerOptions this package actually constructs in NewSink (no
// ReplaceAttr, no JSON handler). The value that follows is RFC3339Nano and so
// never contains a space, which is what lets startupRotate isolate it with a
// plain substring split instead of a full key=value parser.
const slogTimeKeyPrefix = "time="

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
	maxAge   time.Duration
	log      *slog.Logger
}

// NewRotator returns a Rotator that rotates lj every interval, and that also
// rotates a stale current file once at startup (see Run and startupRotate).
// maxAge should be the same age budget lj.MaxAge was derived from
// (config.Config.LogMaxAge in production); a value of zero or less disables
// the startup pass entirely, which callers that construct a Rotator directly
// (rather than through NewSink) may rely on to skip it.
func NewRotator(lj *lumberjack.Logger, interval, maxAge time.Duration, log *slog.Logger) *Rotator {
	return &Rotator{lj: lj, interval: interval, maxAge: maxAge, log: log}
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
// The startup pass calls startupRotate, not rotateOnce directly, both to
// avoid spraying empty rotated backups on a crash-looping agent (see
// startupRotate's own doc comment for the missing/empty-file skip) and,
// since issue #99, to avoid rotating away the very lines the sink is
// currently writing: NewSink opens the file and the caller starts logging
// (self-identification, "agent listening") before this goroutine ever runs,
// so an unconditional startup rotation would move this boot's own opening
// lines into a backup before anything could ever read them from agent.log.
// startupRotate only rotates when the file's existing content is actually
// older than the configured max age.
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
// comment — or when r.maxAge is zero or less, which disables the pass
// entirely.
//
// Otherwise it rotates only when the file's OLDEST content — the timestamp on
// its first line — is already older than r.maxAge. Deliberately not file
// mtime: a crash-looping agent rewrites (or appends to) the file on every
// boot, so mtime is always "just now", and an mtime-based check would never
// fire in exactly the scenario this pass exists for — an agent restarted more
// often than the r.interval ticker. The first line's own timestamp is the
// only signal that reflects how long the content has actually been
// accumulating, so a freshly booted agent's own opening lines are left alone
// and only genuinely stale carry-over content gets rotated aside.
//
// A first line that cannot be read or parsed as a timestamp is treated as
// stale content too: it means the file holds something other than
// recognisable slog output — corrupted mid-write by a prior crash, or written
// by something else entirely — and rotating it aside into a backup preserves
// it while letting agent.log start clean, rather than leaving unreadable
// bytes as the "current" log indefinitely.
func (r *Rotator) startupRotate() {
	if r.maxAge <= 0 {
		return
	}

	info, err := os.Stat(r.lj.Filename)
	if err != nil || info.Size() == 0 {
		return
	}

	ts, err := firstLineTimestamp(r.lj.Filename)
	if err != nil || time.Since(ts) > r.maxAge {
		r.rotateOnce()
	}
}

// firstLineTimestamp reads the first line of path and parses the value that
// follows slogTimeKeyPrefix as an RFC3339Nano timestamp.
func firstLineTimestamp(path string) (time.Time, error) {
	// #nosec G304 -- path is always r.lj.Filename, set once at startup from
	// config.Config.AgentLogPath() (NewSink) or, in tests, a fixed
	// t.TempDir() path. Never a request, a device, or any other untrusted
	// source.
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	line, err := bufio.NewReader(f).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return time.Time{}, fmt.Errorf("read first line of %s: %w", path, err)
	}
	line = strings.TrimRight(line, "\r\n")

	if !strings.HasPrefix(line, slogTimeKeyPrefix) {
		return time.Time{}, fmt.Errorf("first line of %s does not start with %q", path, slogTimeKeyPrefix)
	}
	value := line[len(slogTimeKeyPrefix):]
	if end := strings.IndexByte(value, ' '); end >= 0 {
		value = value[:end]
	}

	ts, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse time from first line of %s: %w", path, err)
	}
	return ts, nil
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
