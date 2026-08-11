// SPDX-License-Identifier: AGPL-3.0-only

// Package logging builds the agent's structured logger over a size- and
// age-bounded file on the state mount, teed to stderr.
//
// Two guarantees this package exists to keep, both PRD requirements:
//
//   - Log lines written before a crash are readable after a restart. That is why
//     the sink is a file on the bind-mounted state directory and not only stderr.
//   - The agent must never be the reason a small VPS runs out of disk. That is
//     why the on-disk total is bounded by whichever of size or age is hit first.
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"

	"gopkg.in/natefinch/lumberjack.v2"

	"github.com/scnplt/devmon-agent/internal/config"
)

// maxBackups is how many rotated files are kept alongside the current one.
//
// The operator states a TOTAL budget, but lumberjack's MaxSize is PER FILE, so
// the total is divided across (maxBackups + 1) files. Setting MaxSize to the
// total directly would give a real footprint of 4x what the operator asked for.
const maxBackups = 3

// logFileMode is applied to the log file after lumberjack creates it. Logs can
// carry device identifiers and request paths, so they are not world-readable.
const logFileMode = 0o600

// logsDirMode keeps the logs directory owner-only, matching the state layout.
const logsDirMode = 0o700

// Sink owns the agent's logger and the file it writes to. It must be closed on
// shutdown so buffered bytes reach disk before the process exits.
type Sink struct {
	Logger  *slog.Logger
	rotator *Rotator
	lj      *lumberjack.Logger
}

// NewSink creates the logs directory, opens the rotated log file, and returns a
// slog logger writing to both that file and stderr.
//
// The stderr tee is not redundant: `docker logs devmon-agent` is the operator's
// first diagnostic, and it only sees the process's standard streams.
func NewSink(cfg config.Config) (*Sink, error) {
	if err := os.MkdirAll(cfg.LogsDir(), logsDirMode); err != nil {
		return nil, fmt.Errorf("create logs dir %s: %w", cfg.LogsDir(), err)
	}

	lj := &lumberjack.Logger{
		Filename:   cfg.AgentLogPath(),
		MaxSize:    perFileMaxMB(cfg.LogMaxTotalMB),
		MaxBackups: maxBackups,
		MaxAge:     int(cfg.LogMaxAge.Hours() / 24),
		Compress:   true,
		LocalTime:  false,
	}

	w := io.MultiWriter(lj, os.Stderr)
	logger := slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: cfg.LogLevel}))

	return &Sink{
		Logger:  logger,
		rotator: NewRotator(lj, defaultRotateInterval, logger),
		lj:      lj,
	}, nil
}

// perFileMaxMB converts the operator's total budget into lumberjack's per-file
// limit. A result of 0 would mean "unlimited" to lumberjack rather than "no
// space", which is why config floors DEVMON_LOG_MAX_TOTAL_MB at 8; the clamp
// here is a second line of defence for any caller that builds a Config directly.
func perFileMaxMB(totalMB int) int {
	if perFile := totalMB / (maxBackups + 1); perFile > 0 {
		return perFile
	}
	return 1
}

// Run drives periodic rotation until ctx is cancelled.
func (s *Sink) Run(ctx context.Context) error { return s.rotator.Run(ctx) }

// Close flushes and closes the log file.
func (s *Sink) Close() error { return s.lj.Close() }

// tightenPermissions is applied by Rotator after each rotation; see rotator.go.
func tightenPermissions(path string) error {
	if err := os.Chmod(path, logFileMode); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("tighten permissions on %s: %w", path, err)
	}
	return nil
}
