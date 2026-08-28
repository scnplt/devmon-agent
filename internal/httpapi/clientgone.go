// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import (
	"context"
	"errors"
	"net"
	"strings"
)

// isClientGone reports whether err — observed while ctx is the request's own
// (possibly derived) context — indicates the peer disconnected rather than a
// genuine service fault. handleStreamContainerLogs uses it for two decisions:
// whether a terminal event: error frame is worth attempting at all once
// StreamContainerLogs has already failed, and, if the frame is attempted,
// what level a failed send should log at. A client that is no longer there
// cannot receive anything the agent tries to tell it, so neither case is an
// agent error.
//
// The checks are ordered from most to least structural, and only the last
// one is a string match:
//
//  1. ctx.Err() != nil, or errors.Is(err, context.Canceled). This is the
//     robust, portable signal: net/http's Request.Context doc comment states
//     the context of an incoming server request "is canceled when the
//     client's connection closes, the request is canceled (with HTTP/2), or
//     when the ServeHTTP method returns" — so this one check already covers
//     both the HTTP/1.1 and HTTP/2 disconnect paths with no string matching
//     at all. handleStreamContainerLogs derives ctx via
//     context.WithCancel(r.Context()) and defers cancel() before this
//     predicate can ever run, so by Go's LIFO defer order the handler's own
//     deferred cancel() has not fired yet at either call site (the streamErr
//     check or the terminal frame write).
//
//     One more source joins the PARENT (r.Context()) being canceled by the
//     client going away: the keepalive goroutine's per-tick revocation
//     re-check (GHSA-qrxm-qm54-xc44) also calls this same cancel() — after
//     writing its own terminal event: error frame carrying msgStreamRevoked
//     — the instant it decides the device's access ended mid-stream. That is
//     deliberate, not a hole in this predicate: by the time StreamContainerLogs
//     unwinds and streamErr reaches isClientGone, the revocation frame has
//     already been delivered, so treating ctx.Err() != nil as "gone" here is
//     exactly what suppresses the second, redundant terminal frame this
//     function guards against.
//
//  2. errors.Is(err, net.ErrClosed). Covers a closed-connection error
//     surfacing through a net.Conn / net.OpError wrapper independently of
//     whether the context has observed the cancellation yet.
//
//  3. A string match on the two disconnect errors actually measured on this
//     route: "client disconnected" and "http2: stream closed". Both come
//     from unexported package-level sentinels inside net/http's bundled
//     HTTP/2 implementation (http2errClientDisconnected and
//     http2errStreamClosed in h2_bundle.go) with no exported equivalent to
//     compare against using errors.Is, so a substring check is genuinely the
//     only option left once arms 1 and 2 do not match — this is a documented
//     fallback, not the primary detector. In practice arm 1 already catches
//     both measured cases (see TestIsClientGone), so this arm exists as a
//     forward-compatible backstop rather than because it is load-bearing
//     today.
//
// syscall.EPIPE and syscall.ECONNRESET were considered and deliberately left
// out: syscall's error constants are not uniform across every platform this
// package must build for (CGO_ENABLED=0, cross-compiled), so checking them
// here would force a build-tag split for a case arm 1 already covers.
func isClientGone(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}

	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return true
	}

	if errors.Is(err, net.ErrClosed) {
		return true
	}

	msg := err.Error()
	return strings.Contains(msg, "client disconnected") || strings.Contains(msg, "http2: stream closed")
}
