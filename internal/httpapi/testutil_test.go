// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import (
	"net"
	"testing"
	"time"
)

// listenerDialTimeout bounds each individual dial attempt in
// waitForListening, so a single hung attempt cannot stall the whole poll
// loop until the overall deadline.
const listenerDialTimeout = 100 * time.Millisecond

// waitForListening dials addr in a short loop until a TCP connection
// succeeds or deadline elapses, replacing a fixed "let the listener bind"
// sleep with synchronization on the listener's actual, observable state.
// A successful dial only proves the OS is accepting connections on addr; it
// does not perform a TLS handshake.
func waitForListening(t *testing.T, addr string, deadline time.Duration) {
	t.Helper()

	giveUpAt := time.Now().Add(deadline)
	for {
		conn, err := net.DialTimeout("tcp", addr, listenerDialTimeout)
		if err == nil {
			_ = conn.Close()
			return
		}
		if time.Now().After(giveUpAt) {
			t.Fatalf("listener at %s never accepted a connection within %s: %v", addr, deadline, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitForCondition polls cond until it reports true or deadline elapses,
// failing the test with the result of msg (evaluated only on timeout, so it
// can report fresh state) on timeout. It replaces ad-hoc "sleep then check"
// patterns with synchronization on the actual state under test.
func waitForCondition(t *testing.T, deadline, pollInterval time.Duration, cond func() bool, msg func() string) {
	t.Helper()

	giveUpAt := time.Now().Add(deadline)
	for {
		if cond() {
			return
		}
		if time.Now().After(giveUpAt) {
			t.Fatal(msg())
		}
		time.Sleep(pollInterval)
	}
}
