// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import (
	"context"
	"errors"
	"net"
	"testing"
)

// TestIsClientGone drives isClientGone over each of its arms directly,
// independent of the HTTP stack, so the predicate's own logic is pinned
// without needing a real disconnected connection.
func TestIsClientGone(t *testing.T) {
	t.Parallel()

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{name: "nil error is never client-gone", ctx: context.Background(), err: nil, want: false},
		{
			name: "canceled context with an unrelated error is client-gone",
			ctx:  canceledCtx,
			err:  errors.New("write: broken pipe"),
			want: true,
		},
		{
			name: "live context but err wraps context.Canceled is client-gone",
			ctx:  context.Background(),
			err:  errors.Join(errors.New("flush sse frame"), context.Canceled),
			want: true,
		},
		{
			name: "err wraps net.ErrClosed is client-gone",
			ctx:  context.Background(),
			err:  errors.Join(errors.New("flush sse frame"), net.ErrClosed),
			want: true,
		},
		{
			name: "err text matches the observed HTTP/2 client-disconnected string",
			ctx:  context.Background(),
			err:  errors.New("flush sse frame: client disconnected"),
			want: true,
		},
		{
			name: "err text matches the observed HTTP/2 stream-closed string",
			ctx:  context.Background(),
			err:  errors.New("flush sse frame: http2: stream closed"),
			want: true,
		},
		{
			name: "a genuine, unrelated failure with a live context is not client-gone",
			ctx:  context.Background(),
			err:  errors.New("engine connection dropped"),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Act
			got := isClientGone(tt.ctx, tt.err)

			// Assert
			if got != tt.want {
				t.Errorf("isClientGone(%v, %v) = %v, want %v", tt.ctx.Err(), tt.err, got, tt.want)
			}
		})
	}
}
