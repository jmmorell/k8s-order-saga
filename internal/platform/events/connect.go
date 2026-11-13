package events

import (
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const connectTimeout = 30 * time.Second

// Connect opens a NATS connection that keeps retrying, and a JetStream handle.
func Connect(url, name string) (*nats.Conn, jetstream.JetStream, error) {
	nc, err := nats.Connect(url,
		nats.Name(name),
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		// Fail publishes at once while disconnected instead of buffering them,
		// so an order rejected with 503 is never published after a reconnect.
		nats.ReconnectBufSize(-1),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("connect to nats: %w", err)
	}
	// RetryOnFailedConnect returns before the first connection is up, and without a
	// reconnect buffer every call made in that window would fail. Wait for it.
	deadline := time.Now().Add(connectTimeout)
	for !nc.IsConnected() {
		if time.Now().After(deadline) {
			nc.Close()
			return nil, nil, fmt.Errorf("connect to nats: not connected after %s", connectTimeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, nil, fmt.Errorf("create jetstream: %w", err)
	}
	return nc, js, nil
}
