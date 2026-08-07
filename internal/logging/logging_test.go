package logging

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestRotatorRotatesOnTick(t *testing.T) {
	t.Parallel()

	// Arrange — a short interval stands in for the production 24h one.
	cfg := testConfig(t, 8)
	s, err := NewSink(cfg)
	if err != nil {
		t.Fatalf("NewSink() unexpected error: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	s.Logger.Info("seed line so there is something to rotate")

	r := NewRotator(s.lj, 10*time.Millisecond, s.Logger)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Act
	go func() { _ = r.Run(ctx) }()

	// Assert
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		entries, _ := filepath.Glob(filepath.Join(cfg.LogsDir(), "agent-*"))
		if len(entries) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("ticker never produced a rotated file; age-based retention would never apply")
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
