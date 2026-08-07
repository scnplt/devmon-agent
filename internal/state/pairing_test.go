package state

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMintPairingCodeThenRedeemSucceeds(t *testing.T) {
	t.Parallel()

	// Arrange
	s := openStore(t, tempDBPath(t))
	ctx := context.Background()
	code, expiresAt, err := s.MintPairingCode(ctx, "Pixel 9")
	if err != nil {
		t.Fatalf("MintPairingCode() unexpected error: %v", err)
	}
	if expiresAt.Before(time.Now()) {
		t.Fatalf("MintPairingCode() expiresAt = %v, want a time in the future", expiresAt)
	}

	// Act
	name, err := s.RedeemPairingCode(ctx, code, "device-1")

	// Assert
	if err != nil {
		t.Fatalf("RedeemPairingCode() unexpected error: %v", err)
	}
	if name != "Pixel 9" {
		t.Errorf("RedeemPairingCode() deviceName = %q, want %q", name, "Pixel 9")
	}
}

func TestRedeemPairingCodeTwiceFailsSecondTime(t *testing.T) {
	t.Parallel()

	// Arrange
	s := openStore(t, tempDBPath(t))
	ctx := context.Background()
	code, _, err := s.MintPairingCode(ctx, "Pixel 9")
	if err != nil {
		t.Fatalf("MintPairingCode() unexpected error: %v", err)
	}
	if _, err := s.RedeemPairingCode(ctx, code, "device-1"); err != nil {
		t.Fatalf("first RedeemPairingCode() unexpected error: %v", err)
	}

	// Act
	_, err = s.RedeemPairingCode(ctx, code, "device-2")

	// Assert
	if !errors.Is(err, ErrPairingCodeInvalid) {
		t.Fatalf("second RedeemPairingCode() error = %v, want ErrPairingCodeInvalid", err)
	}
}

func TestRedeemPairingCodeUnknownFails(t *testing.T) {
	t.Parallel()

	// Arrange
	s := openStore(t, tempDBPath(t))

	// Act
	_, err := s.RedeemPairingCode(context.Background(), "never-minted-code", "device-1")

	// Assert
	if !errors.Is(err, ErrPairingCodeInvalid) {
		t.Fatalf("RedeemPairingCode() error = %v, want ErrPairingCodeInvalid", err)
	}
}

func TestRedeemPairingCodeExpiredFails(t *testing.T) {
	t.Parallel()

	// Arrange — mint, then push expires_at into the past directly, since
	// there is no way to observe a code expiring through the public API
	// alone within a fast test.
	s := openStore(t, tempDBPath(t))
	ctx := context.Background()
	code, _, err := s.MintPairingCode(ctx, "Pixel 9")
	if err != nil {
		t.Fatalf("MintPairingCode() unexpected error: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE pairing_codes SET expires_at = ?`, time.Now().Add(-time.Minute).Unix(),
	); err != nil {
		t.Fatalf("seed expired pairing code: %v", err)
	}

	// Act
	_, err = s.RedeemPairingCode(ctx, code, "device-1")

	// Assert
	if !errors.Is(err, ErrPairingCodeInvalid) {
		t.Fatalf("RedeemPairingCode() error = %v, want ErrPairingCodeInvalid", err)
	}
}

func TestRedeemPairingCodeConcurrentProducesExactlyOneSuccess(t *testing.T) {
	t.Parallel()

	// Arrange — this test is the whole justification for the conditional
	// UPDATE ... RETURNING in RedeemPairingCode: a read-then-write pair
	// would let more than one goroutine observe the code as unused.
	s := openStore(t, tempDBPath(t))
	ctx := context.Background()
	code, _, err := s.MintPairingCode(ctx, "Pixel 9")
	if err != nil {
		t.Fatalf("MintPairingCode() unexpected error: %v", err)
	}

	const goroutines = 10
	var (
		successes int64
		failures  int64
		wg        sync.WaitGroup
	)
	wg.Add(goroutines)
	for i := range goroutines {
		go func(n int) {
			defer wg.Done()
			deviceID := "device-" + string(rune('a'+n))
			_, err := s.RedeemPairingCode(ctx, code, deviceID)
			switch {
			case err == nil:
				atomic.AddInt64(&successes, 1)
			case errors.Is(err, ErrPairingCodeInvalid):
				atomic.AddInt64(&failures, 1)
			default:
				t.Errorf("RedeemPairingCode() unexpected error: %v", err)
			}
		}(i)
	}
	wg.Wait()

	// Assert
	if successes != 1 {
		t.Errorf("RedeemPairingCode() successes = %d, want exactly 1", successes)
	}
	if failures != goroutines-1 {
		t.Errorf("RedeemPairingCode() failures = %d, want %d", failures, goroutines-1)
	}
}

func TestPrunePairingCodesRemovesExpiredRows(t *testing.T) {
	t.Parallel()

	// Arrange
	s := openStore(t, tempDBPath(t))
	ctx := context.Background()
	if _, _, err := s.MintPairingCode(ctx, "Expired Phone"); err != nil {
		t.Fatalf("MintPairingCode() unexpected error: %v", err)
	}
	if _, _, err := s.MintPairingCode(ctx, "Active Phone"); err != nil {
		t.Fatalf("MintPairingCode() unexpected error: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE pairing_codes SET expires_at = ? WHERE device_name = ?`,
		time.Now().Add(-time.Minute).Unix(), "Expired Phone",
	); err != nil {
		t.Fatalf("seed expired pairing code: %v", err)
	}

	// Act
	pruned, err := s.PrunePairingCodes(ctx)

	// Assert
	if err != nil {
		t.Fatalf("PrunePairingCodes() unexpected error: %v", err)
	}
	if pruned != 1 {
		t.Errorf("PrunePairingCodes() pruned = %d, want 1", pruned)
	}
}
