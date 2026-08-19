// SPDX-License-Identifier: AGPL-3.0-only

package logging

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"

	"github.com/scnplt/devmon-agent/internal/config"
)

func testConfig(t *testing.T, totalMB int) config.Config {
	t.Helper()
	return config.Config{
		StateDir:      t.TempDir(),
		LogLevel:      slog.LevelInfo,
		LogMaxAge:     24 * time.Hour,
		LogMaxTotalMB: totalMB,
	}
}

// waitForGlobMatch polls pattern until it matches at least one file or
// deadline elapses, so a test can synchronize on a background ticker's
// observable effect (a rotated file appearing on disk) instead of sleeping
// a fixed amount of wall-clock time.
func waitForGlobMatch(t *testing.T, pattern string, deadline time.Duration) []string {
	t.Helper()

	giveUpAt := time.Now().Add(deadline)
	for {
		entries, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatalf("glob %q: %v", pattern, err)
		}
		if len(entries) > 0 {
			return entries
		}
		if time.Now().After(giveUpAt) {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestSinkPersistsAcrossReopen is the phase's crash-survival signal: lines
// written by one process must still be readable after it dies and a new one
// opens the same state mount.
func TestSinkPersistsAcrossReopen(t *testing.T) {
	t.Parallel()

	// Arrange
	cfg := testConfig(t, 8)

	// Act — first "process" writes and exits.
	first, err := NewSink(cfg)
	if err != nil {
		t.Fatalf("NewSink() unexpected error: %v", err)
	}
	first.Logger.Info("before the crash", slog.String("marker", "run-one"))
	if err := first.Close(); err != nil {
		t.Fatalf("Close() unexpected error: %v", err)
	}

	// A second "process" opens the same directory and appends.
	second, err := NewSink(cfg)
	if err != nil {
		t.Fatalf("NewSink() second unexpected error: %v", err)
	}
	second.Logger.Info("after the restart", slog.String("marker", "run-two"))
	if err := second.Close(); err != nil {
		t.Fatalf("Close() second unexpected error: %v", err)
	}

	// Assert
	data, err := os.ReadFile(cfg.AgentLogPath())
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	got := string(data)
	for _, want := range []string{"run-one", "run-two"} {
		if !strings.Contains(got, want) {
			t.Errorf("log does not contain %q; got:\n%s", want, got)
		}
	}
	if strings.Index(got, "run-one") > strings.Index(got, "run-two") {
		t.Error("second run's line precedes the first; the file was truncated rather than appended")
	}
}

// TestPerFileBudget guards the single most likely bug in this package: treating
// lumberjack's per-file MaxSize as a total budget, and thereby using 4x the disk
// the operator asked for.
func TestPerFileBudget(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		totalMB int
		want    int
	}{
		{name: "config floor divides evenly", totalMB: 8, want: 2},
		{name: "default budget", totalMB: 64, want: 16},
		{name: "remainder is truncated, never rounded up", totalMB: 11, want: 2},
		{name: "below the floor clamps to 1 rather than unlimited", totalMB: 2, want: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Arrange
			cfg := testConfig(t, tt.totalMB)

			// Act
			s, err := NewSink(cfg)
			if err != nil {
				t.Fatalf("NewSink() unexpected error: %v", err)
			}
			t.Cleanup(func() { _ = s.Close() })

			// Assert
			if s.lj.MaxSize != tt.want {
				t.Errorf("lj.MaxSize = %d, want %d (total %d MB over %d files)",
					s.lj.MaxSize, tt.want, tt.totalMB, maxBackups+1)
			}
		})
	}
}

func TestSinkLumberjackSettings(t *testing.T) {
	t.Parallel()

	// Arrange
	cfg := testConfig(t, 64)
	cfg.LogMaxAge = 7 * 24 * time.Hour

	// Act
	s, err := NewSink(cfg)
	if err != nil {
		t.Fatalf("NewSink() unexpected error: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// Assert
	if s.lj.MaxAge != 7 {
		t.Errorf("lj.MaxAge = %d, want 7 days", s.lj.MaxAge)
	}
	if s.lj.MaxBackups != maxBackups {
		t.Errorf("lj.MaxBackups = %d, want %d", s.lj.MaxBackups, maxBackups)
	}
	if !s.lj.Compress {
		t.Error("lj.Compress = false, want true so rotated files cost less disk")
	}
	if s.lj.LocalTime {
		t.Error("lj.LocalTime = true, want false so rotated names are UTC")
	}
}

// TestRotateProducesBackup exercises the path the Rotator ticker drives, since
// waiting 24h for a real tick is not a test.
func TestRotateProducesBackup(t *testing.T) {
	t.Parallel()

	// Arrange
	cfg := testConfig(t, 8)
	s, err := NewSink(cfg)
	if err != nil {
		t.Fatalf("NewSink() unexpected error: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	// lumberjack starts a background "mill" goroutine on the first rotation to
	// compress rotated files, and Close never stops or waits for it. With
	// compression on, that goroutine can still be writing a .gz into the logs
	// dir after this test returns and t.TempDir removes it, which fails
	// cleanup with "directory not empty". Compression itself isn't the
	// behaviour under test here, so turn it off.
	s.lj.Compress = false
	s.Logger.Info("line before rotation")

	// Act
	s.rotator.rotateOnce()
	s.Logger.Info("line after rotation")

	// Assert — the current file plus at least one rotated artefact.
	entries, err := filepath.Glob(filepath.Join(cfg.LogsDir(), "agent*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(entries) < 2 {
		t.Fatalf("found %v, want the current log plus a rotated backup", entries)
	}
}

func TestRotatorRunStopsOnContextCancel(t *testing.T) {
	t.Parallel()

	// Arrange
	cfg := testConfig(t, 8)
	s, err := NewSink(cfg)
	if err != nil {
		t.Fatalf("NewSink() unexpected error: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)

	// Act
	go func() { done <- s.Run(ctx) }()
	cancel()

	// Assert
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Errorf("Run() = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after the context was cancelled")
	}
}

// TestRotatorRunRotatesOnStartupWhenFileHasContent is the regression test for
// issue #42: a Rotator that has just started must not wait a full interval
// (24h in production) before its first pass, or DEVMON_LOG_MAX_AGE_DAYS is
// never enforced on a host that restarts more often than that.
func TestRotatorRunRotatesOnStartupWhenFileHasContent(t *testing.T) {
	t.Parallel()

	// Arrange — a real log line means the current file is non-empty, the
	// shape a freshly booted agent's log is in by the time Run starts (see
	// serve() in cmd/devmon-agent: several lines are logged before runAll
	// starts the rotator).
	cfg := testConfig(t, 8)
	s, err := NewSink(cfg)
	if err != nil {
		t.Fatalf("NewSink() unexpected error: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	s.lj.Compress = false
	s.Logger.Info("line written before Run starts")

	// Arrange — interval far longer than the test timeout proves any rotated
	// backup found below cannot be explained by a tick firing.
	r := NewRotator(s.lj, time.Hour, s.Logger)
	runCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	done := make(chan struct{})

	// Act
	go func() {
		defer close(done)
		_ = r.Run(runCtx)
	}()

	// Assert
	entries := waitForGlobMatch(t, filepath.Join(cfg.LogsDir(), "agent-*"), 2*time.Second)
	cancel()
	<-done
	if len(entries) == 0 {
		t.Error("Run() never produced a rotated backup within the test window; the startup pass regressed")
	}
}

// TestRotatorRunSkipsStartupRotationOnEmptyOrMissingFile guards against the
// behavior the issue warned about: a crash-looping agent must not spray
// empty rotated backups on every boot. lumberjack's Rotate (via openNew)
// stats the current filename and, when it does not exist, creates it fresh
// with no rename at all — that path alone is harmless. The unsafe case is a
// current file that exists but is empty: Rotate would still rename it aside,
// producing a useless backup file. Verified by reading
// gopkg.in/natefinch/lumberjack.v2's openNew: it renames whenever os.Stat on
// the current filename succeeds, regardless of size. The startup pass must
// therefore skip rotation itself whenever the current file is missing or
// zero bytes, rather than relying on lumberjack.
func TestRotatorRunSkipsStartupRotationOnEmptyOrMissingFile(t *testing.T) {
	t.Parallel()

	// Arrange — a fresh sink whose current log file has not been written to
	// yet, so it does not exist on disk (lumberjack opens lazily on first
	// Write).
	cfg := testConfig(t, 8)
	s, err := NewSink(cfg)
	if err != nil {
		t.Fatalf("NewSink() unexpected error: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	s.lj.Compress = false

	r := NewRotator(s.lj, time.Hour, s.Logger)
	runCtx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	done := make(chan struct{})

	// Act
	go func() {
		defer close(done)
		_ = r.Run(runCtx)
	}()
	<-runCtx.Done()
	cancel()
	<-done

	// Assert — no current file and no backup: the startup pass must not have
	// invented one.
	entries, err := filepath.Glob(filepath.Join(cfg.LogsDir(), "agent*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("logs dir contains %v after a startup pass on an unwritten sink, want no files", entries)
	}
}

// TestRotatorRunDoesNothingWhenContextAlreadyCancelled proves the startup
// pass is skipped entirely when ctx is already dead on entry.
func TestRotatorRunDoesNothingWhenContextAlreadyCancelled(t *testing.T) {
	t.Parallel()

	// Arrange
	cfg := testConfig(t, 8)
	s, err := NewSink(cfg)
	if err != nil {
		t.Fatalf("NewSink() unexpected error: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	s.lj.Compress = false
	s.Logger.Info("line written before Run is ever called")

	r := NewRotator(s.lj, time.Hour, s.Logger)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Act
	err = r.Run(ctx)

	// Assert
	if err != context.Canceled {
		t.Errorf("Run() = %v, want context.Canceled", err)
	}
	entries, globErr := filepath.Glob(filepath.Join(cfg.LogsDir(), "agent-*"))
	if globErr != nil {
		t.Fatalf("glob: %v", globErr)
	}
	if len(entries) != 0 {
		t.Errorf("logs dir contains rotated backups %v, want none: Run() must not rotate when ctx is already cancelled", entries)
	}
}

func TestRotatorRotatesOnTick(t *testing.T) {
	t.Parallel()

	// Arrange — a short interval stands in for the production 24h one.
	cfg := testConfig(t, 8)
	s, err := NewSink(cfg)
	if err != nil {
		t.Fatalf("NewSink() unexpected error: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	// lumberjack starts a background "mill" goroutine on the first rotation to
	// compress rotated files, and Close never stops or waits for it. With
	// compression on and a 10ms tick, that goroutine can still be writing a
	// .gz into the logs dir after t.TempDir starts removing it, which fails
	// cleanup with "directory not empty". Compression itself isn't the
	// behaviour under test here (the ticker driving rotation is), so turn it
	// off.
	s.lj.Compress = false
	s.Logger.Info("seed line so there is something to rotate")

	r := NewRotator(s.lj, 10*time.Millisecond, s.Logger)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	// The test returns as soon as it sees one rotated file, but the ticker
	// keeps firing until it is told to stop. Cancelling is not enough: a tick
	// already inside rotateOnce would create a file while t.TempDir is removing
	// the directory, which fails the run with "directory not empty". Registered
	// after the cleanups above so LIFO waits for the goroutine before either.
	done := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		<-done
	})

	// Act
	go func() {
		defer close(done)
		_ = r.Run(ctx)
	}()

	// Assert
	if entries := waitForGlobMatch(t, filepath.Join(cfg.LogsDir(), "agent-*"), 2*time.Second); len(entries) == 0 {
		t.Error("ticker never produced a rotated file; age-based retention would never apply")
	}
}

// TestRotateOnceLogsErrorWhenRotateFails covers rotateOnce's error branch.
// lumberjack.Logger.Rotate opens its target with os.MkdirAll(dir, ...) first;
// pointing Filename at a path whose parent segment is an ordinary file — not
// a missing or extra directory — makes that MkdirAll fail identically on
// every OS, unlike a directory-as-filename trick which some platforms
// silently work around by renaming the directory itself. rotateOnce must log
// the failure and return without panicking or attempting
// tightenPermissions.
func TestRotateOnceLogsErrorWhenRotateFails(t *testing.T) {
	t.Parallel()

	// Arrange — blocker is a regular file, so the "sub" directory Rotate
	// needs to create underneath it can never exist.
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker file: %v", err)
	}
	lj := &lumberjack.Logger{Filename: filepath.Join(blocker, "sub", "agent.log")}
	log, buf := newCapturingLoggerForTest()
	r := NewRotator(lj, defaultRotateInterval, log)

	// Act — must not panic.
	r.rotateOnce()

	// Assert
	if !strings.Contains(buf.String(), "log rotation failed") {
		t.Errorf("log = %q, want it to mention the rotation failure", buf.String())
	}
}

// TestTightenPermissionsIgnoresMissingFile covers tightenPermissions'
// success-on-absence branch: a rotated-away file that no longer exists is
// not an error worth reporting, since the mode only matters for the file
// that currently exists.
func TestTightenPermissionsIgnoresMissingFile(t *testing.T) {
	t.Parallel()

	// Arrange
	missing := filepath.Join(t.TempDir(), "does-not-exist.log")

	// Act
	err := tightenPermissions(missing)

	// Assert
	if err != nil {
		t.Errorf("tightenPermissions(%q) error = %v, want nil for a missing file", missing, err)
	}
}

// TestTightenPermissionsWrapsRealChmodFailure covers tightenPermissions'
// error branch: a path Chmod rejects for a reason other than "does not
// exist" — here, an embedded NUL byte, which every OS's path syscalls reject
// as invalid rather than as a missing file — must return a wrapped error
// naming the path.
func TestTightenPermissionsWrapsRealChmodFailure(t *testing.T) {
	t.Parallel()

	// Arrange
	invalid := filepath.Join(t.TempDir(), "bad\x00name.log")

	// Act
	err := tightenPermissions(invalid)

	// Assert
	if err == nil {
		t.Fatal("tightenPermissions() error = nil, want a failure for an invalid path")
	}
	if !strings.Contains(err.Error(), "tighten permissions") {
		t.Errorf("error %q does not identify the failing step", err)
	}
}

// newCapturingLoggerForTest mirrors the pattern used across this repo's other
// packages: a text-handler logger writing into a buffer a test can inspect.
func newCapturingLoggerForTest() (*slog.Logger, *strings.Builder) {
	var buf strings.Builder
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

func TestNewSinkFailsOnUncreatableLogsDir(t *testing.T) {
	t.Parallel()

	// Arrange — a regular file where the state directory should be, so MkdirAll
	// cannot succeed. This is the shape of the "state dir not writable" failure.
	blocker := filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	cfg := config.Config{StateDir: blocker, LogMaxTotalMB: 8, LogMaxAge: 24 * time.Hour}

	// Act
	_, err := NewSink(cfg)

	// Assert
	if err == nil {
		t.Fatal("NewSink() error = nil, want a failure naming the logs dir")
	}
	if !strings.Contains(err.Error(), "create logs dir") {
		t.Errorf("error %q does not identify the failing step", err)
	}
}
