//go:build e2e

package harness

import (
	"fmt"
	"io"
	"net"
	"net/url"
	"sync"
	"testing"
)

// Proxy is a plain TCP listener that forwards every accepted connection,
// bidirectionally, to a real Docker Engine endpoint sitting behind it. Sever
// closes the listener and every connection currently in flight through it,
// simulating an Engine that vanished mid-request; Restore reopens on the
// exact same port so an agent already configured with this proxy's address
// recovers reachability with no restart.
//
// This is D16's answer to the Engine-unavailable 502 path: stopping the
// developer's actual Docker daemon to exercise it would take down every other
// test in the suite that still needs a live Engine, on a host that has
// nothing to do with this test run. A Go proxy in front of the real endpoint
// is severable on command and touches nothing else.
//
// The proxy MUST be listening before the agent under test starts:
// dockerx.New pings the Engine at startup, and a dead endpoint at that moment
// is a fatal startup error, not the 502 this proxy exists to produce.
type Proxy struct {
	upstreamNetwork string
	upstreamAddr    string

	mu     sync.Mutex
	ln     net.Listener
	port   int
	closed bool
	conns  map[net.Conn]struct{}
	wg     sync.WaitGroup
}

// NewProxy starts listening on a fresh 127.0.0.1 port and forwards every
// accepted connection to upstreamHost — an Engine endpoint in the same
// unix://... or tcp://... form internal/config's DEVMON_DOCKER_HOST accepts.
// It registers a t.Cleanup that severs the proxy for good at test end.
func NewProxy(t *testing.T, upstreamHost string) *Proxy {
	t.Helper()

	network, addr, err := parseEngineHost(upstreamHost)
	if err != nil {
		t.Fatalf("proxy: %v", err)
	}

	p := &Proxy{
		upstreamNetwork: network,
		upstreamAddr:    addr,
		conns:           make(map[net.Conn]struct{}),
	}
	p.listen(t)
	t.Cleanup(p.shutdown)
	return p
}

// Addr returns the tcp://127.0.0.1:<port> value to hand the agent under test
// as its DEVMON_DOCKER_HOST (via AgentOptions.DockerHost).
func (p *Proxy) Addr() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return fmt.Sprintf("tcp://127.0.0.1:%d", p.port)
}

// listen opens (or, from Restore, reopens on the same port) the proxy's
// listener and starts its accept loop.
func (p *Proxy) listen(t *testing.T) {
	t.Helper()

	p.mu.Lock()
	port := p.port
	p.mu.Unlock()

	addr := "127.0.0.1:0"
	if port != 0 {
		addr = fmt.Sprintf("127.0.0.1:%d", port)
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("proxy: listen %s: %v", addr, err)
	}
	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("proxy: unexpected listener address type %T", ln.Addr())
	}

	p.mu.Lock()
	p.ln = ln
	p.port = tcpAddr.Port
	p.closed = false
	p.mu.Unlock()

	go p.acceptLoop(ln)
}

// acceptLoop runs until ln is closed, either by Sever or by the test's final
// t.Cleanup teardown.
func (p *Proxy) acceptLoop(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		p.wg.Add(1)
		go p.forward(conn)
	}
}

// forward dials the real Engine and copies bytes both ways until either side
// closes. A dial failure closes the accepted connection immediately, the same
// outward behaviour a client sees when the real endpoint itself refuses.
func (p *Proxy) forward(conn net.Conn) {
	defer p.wg.Done()

	upstream, err := net.Dial(p.upstreamNetwork, p.upstreamAddr)
	if err != nil {
		_ = conn.Close()
		return
	}

	p.track(conn, upstream)
	defer p.untrack(conn, upstream)

	var pipes sync.WaitGroup
	pipes.Add(2)
	go func() { defer pipes.Done(); _, _ = io.Copy(upstream, conn) }()
	go func() { defer pipes.Done(); _, _ = io.Copy(conn, upstream) }()
	pipes.Wait()
}

func (p *Proxy) track(conns ...net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range conns {
		p.conns[c] = struct{}{}
	}
}

func (p *Proxy) untrack(conns ...net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range conns {
		delete(p.conns, c)
		_ = c.Close()
	}
}

// Sever closes the listener and every connection currently forwarding
// through it — the primitive the 502 tests need, without stopping the
// developer's own Docker daemon (D16). It blocks until every in-flight
// forward goroutine has actually returned, so the caller's next request is
// guaranteed to hit a closed port, not a race with a connection still being
// torn down.
func (p *Proxy) Sever(t *testing.T) {
	t.Helper()

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	ln := p.ln
	conns := make([]net.Conn, 0, len(p.conns))
	for c := range p.conns {
		conns = append(conns, c)
	}
	p.mu.Unlock()

	if ln != nil {
		if err := ln.Close(); err != nil {
			t.Logf("proxy: close listener: %v", err)
		}
	}
	for _, c := range conns {
		_ = c.Close()
	}
	p.wg.Wait()
}

// Restore reopens the listener on the exact port Sever closed, so the agent
// — already configured with this proxy's address — recovers reachability
// with no restart, the same way a real Docker daemon coming back up would.
func (p *Proxy) Restore(t *testing.T) {
	t.Helper()

	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if !closed {
		t.Fatalf("proxy: Restore called on a proxy that was never severed")
	}
	p.listen(t)
}

// shutdown is the t.Cleanup body: it severs the proxy for good and does not
// reopen it. It never calls t.Fatalf — cleanup functions run after the test
// itself may have already finished, and a background failure here would have
// nowhere useful to report.
func (p *Proxy) shutdown() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	ln := p.ln
	p.mu.Unlock()

	if ln != nil {
		_ = ln.Close()
	}
	p.wg.Wait()
}

// parseEngineHost turns an internal/config-shaped DEVMON_DOCKER_HOST value
// (unix://... or tcp://...) into the network and address net.Dial expects.
// internal/config.go accepts only these two schemes (D6), so the proxy needs
// to understand only these two.
func parseEngineHost(host string) (network, address string, err error) {
	u, err := url.Parse(host)
	if err != nil {
		return "", "", fmt.Errorf("parse docker host %q: %w", host, err)
	}
	switch u.Scheme {
	case "unix":
		return "unix", u.Path, nil
	case "tcp":
		return "tcp", u.Host, nil
	default:
		return "", "", fmt.Errorf("unsupported docker host scheme %q in %q", u.Scheme, host)
	}
}
