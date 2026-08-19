// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scnplt/devmon-agent/internal/config"
)

func testStateConfig(t *testing.T) config.Config {
	t.Helper()
	return config.Config{StateDir: t.TempDir()}
}

func statDir(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("is not a directory")
	}
	return nil
}

// runAll fans out the long-lived components and is the one place a mistake
// hangs the agent instead of failing it, so each case below asserts that it
// RETURNS — a deadlock here would otherwise show up as a container that never
// stops and has to be SIGKILLed, corrupting the WAL.
const runAllTimeout = 5 * time.Second

// blockUntilCancelled models a well-behaved component: it runs until the
// context is cancelled, then reports why it stopped.
func blockUntilCancelled(started *atomic.Int32) func(context.Context) error {
	return func(ctx context.Context) error {
		started.Add(1)
		<-ctx.Done()
		return ctx.Err()
	}
}

// waitForRunAll runs runAll and fails the test if it does not return in time.
func waitForRunAll(t *testing.T, ctx context.Context, components ...func(context.Context) error) error {
	t.Helper()

	done := make(chan error, 1)
	go func() { done <- runAll(ctx, components...) }()

	select {
	case err := <-done:
		return err
	case <-time.After(runAllTimeout):
		t.Fatal("runAll did not return; the agent would hang instead of shutting down")
		return nil
	}
}

// waitForStartedCount polls started until it reaches want or deadline
// elapses, so a test can synchronize on components actually having run
// instead of sleeping a fixed amount of wall-clock time before cancelling.
func waitForStartedCount(t *testing.T, started *atomic.Int32, want int32, deadline time.Duration) {
	t.Helper()

	giveUpAt := time.Now().Add(deadline)
	for {
		if got := started.Load(); got >= want {
			return
		}
		if time.Now().After(giveUpAt) {
			t.Fatalf("started count = %d after %s, want >= %d", started.Load(), deadline, want)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRunAllReturnsNilOnCleanShutdown(t *testing.T) {
	t.Parallel()

	// Arrange — three components that stop only when the context is cancelled,
	// as SIGTERM does in production.
	var started atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runAll(ctx,
			blockUntilCancelled(&started),
			blockUntilCancelled(&started),
			blockUntilCancelled(&started),
		)
	}()

	// Act — wait on the test's own goroutine (t.Fatal is unsafe from a
	// spawned one) for all three components to have actually started before
	// cancelling, rather than sleeping a fixed guess at how long that takes.
	waitForStartedCount(t, &started, 3, 2*time.Second)
	cancel()

	var err error
	select {
	case err = <-done:
	case <-time.After(runAllTimeout):
		t.Fatal("runAll did not return; the agent would hang instead of shutting down")
	}

	// Assert — context.Canceled is how a clean shutdown reaches these
	// components. Surfacing it would make every SIGTERM exit non-zero and
	// Docker would record the container as failed.
	if err != nil {
		t.Errorf("runAll() = %v, want nil after a cancelled context", err)
	}
	if n := started.Load(); n != 3 {
		t.Errorf("%d of 3 components started", n)
	}
}

// TestRunAllStopsSiblingsOnFailure is the regression guard for the deadlock this
// function was originally written with: without cancelling on the first return,
// a component that fails immediately leaves the others running forever and the
// wait never finishes.
func TestRunAllStopsSiblingsOnFailure(t *testing.T) {
	t.Parallel()

	// Arrange — one component fails at once, two would otherwise run forever.
	wantErr := errors.New("listener could not bind")
	var started atomic.Int32

	failing := func(context.Context) error { return wantErr }

	// Act
	err := waitForRunAll(t, context.Background(),
		blockUntilCancelled(&started),
		failing,
		blockUntilCancelled(&started),
	)

	// Assert
	if !errors.Is(err, wantErr) {
		t.Errorf("runAll() = %v, want %v", err, wantErr)
	}
}

func TestRunAllReportsFailureFromAnyPosition(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("component failed")

	tests := []struct {
		name     string
		position int
	}{
		{name: "first component fails", position: 0},
		{name: "middle component fails", position: 1},
		{name: "last component fails", position: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Arrange
			var started atomic.Int32
			components := make([]func(context.Context) error, 3)
			for i := range components {
				if i == tt.position {
					components[i] = func(context.Context) error { return wantErr }
					continue
				}
				components[i] = blockUntilCancelled(&started)
			}

			// Act
			err := waitForRunAll(t, context.Background(), components...)

			// Assert
			if !errors.Is(err, wantErr) {
				t.Errorf("runAll() = %v, want %v", err, wantErr)
			}
		})
	}
}

// TestRunAllPrefersRealFailureOverCancellation guards the error-selection loop:
// once one component fails, the others return context.Canceled, and the real
// cause must not be lost among them.
func TestRunAllPrefersRealFailureOverCancellation(t *testing.T) {
	t.Parallel()

	// Arrange — the failing component is last. Ordering here does not depend
	// on wall-clock timing: runAll sends a component's error to the shared
	// errs channel before its own deferred cancel() runs, and the other two
	// components are blocked on <-ctx.Done() until some component calls
	// cancel(). So slowFail's error is always queued before the siblings'
	// cancellation errors can be, regardless of how quickly it returns.
	wantErr := errors.New("the real cause")
	var started atomic.Int32
	slowFail := func(context.Context) error { return wantErr }

	// Act
	err := waitForRunAll(t, context.Background(),
		blockUntilCancelled(&started),
		blockUntilCancelled(&started),
		slowFail,
	)

	// Assert
	if !errors.Is(err, wantErr) {
		t.Errorf("runAll() = %v, want %v; a cancellation masked the real failure", err, wantErr)
	}
}

func TestRunAllWithNoComponents(t *testing.T) {
	t.Parallel()

	// Arrange / Act — degenerate case: must return rather than block on an
	// empty WaitGroup channel.
	err := waitForRunAll(t, context.Background())

	// Assert
	if err != nil {
		t.Errorf("runAll() = %v, want nil", err)
	}
}

func TestPrepareStateDirCreatesLayout(t *testing.T) {
	t.Parallel()

	// Arrange
	cfg := testStateConfig(t)

	// Act
	if err := prepareStateDir(cfg); err != nil {
		t.Fatalf("prepareStateDir() unexpected error: %v", err)
	}

	// Assert
	for _, dir := range []string{cfg.StateDir, cfg.CertsDir(), cfg.LogsDir()} {
		if err := statDir(dir); err != nil {
			t.Errorf("%s: %v", dir, err)
		}
	}
}

func TestPrepareStateDirIsIdempotent(t *testing.T) {
	t.Parallel()

	// Arrange — a restart re-runs this against an existing state directory.
	cfg := testStateConfig(t)
	if err := prepareStateDir(cfg); err != nil {
		t.Fatalf("first prepareStateDir() unexpected error: %v", err)
	}

	// Act
	err := prepareStateDir(cfg)

	// Assert
	if err != nil {
		t.Errorf("second prepareStateDir() = %v, want nil", err)
	}
}
