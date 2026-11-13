package testinfra

import (
	"net"
	"sync"
	"testing"
)

// Proxy is a TCP forwarder that can be cut and restored on the same address,
// which makes a client connection disconnected rather than closed.
type Proxy struct {
	t      *testing.T
	addr   string
	target string

	mu    sync.Mutex
	ln    net.Listener
	conns []net.Conn
}

// NATSBehindProxy starts a NATS server behind a Proxy and returns the proxy URL.
func NATSBehindProxy(t *testing.T) (url string, p *Proxy) {
	t.Helper()
	direct := NATS(t)
	target := direct[len("nats://"):]
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	p = &Proxy{t: t, addr: ln.Addr().String(), target: target}
	p.serve(ln)
	t.Cleanup(p.Down)
	return "nats://" + p.addr, p
}

func (p *Proxy) serve(ln net.Listener) {
	p.mu.Lock()
	p.ln = ln
	p.mu.Unlock()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			up, err := net.Dial("tcp", p.target)
			if err != nil {
				_ = c.Close()
				continue
			}
			p.mu.Lock()
			p.conns = append(p.conns, c, up)
			p.mu.Unlock()
			go func() { _, _ = copyAll(up, c); _ = up.Close(); _ = c.Close() }()
			go func() { _, _ = copyAll(c, up); _ = up.Close(); _ = c.Close() }()
		}
	}()
}

// Down stops accepting and drops every open connection.
func (p *Proxy) Down() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ln != nil {
		_ = p.ln.Close()
		p.ln = nil
	}
	for _, c := range p.conns {
		_ = c.Close()
	}
	p.conns = nil
}

// Up listens again on the original address.
func (p *Proxy) Up() {
	p.t.Helper()
	ln, err := net.Listen("tcp", p.addr)
	if err != nil {
		p.t.Fatalf("relisten: %v", err)
	}
	p.serve(ln)
}

func copyAll(dst, src net.Conn) (int64, error) {
	buf := make([]byte, 32<<10)
	var n int64
	for {
		r, err := src.Read(buf)
		if r > 0 {
			w, werr := dst.Write(buf[:r])
			n += int64(w)
			if werr != nil {
				return n, werr
			}
		}
		if err != nil {
			return n, err
		}
	}
}
