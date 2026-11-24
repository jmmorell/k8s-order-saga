package events_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/jmmorell/k8s-order-saga/internal/platform/events"
	"github.com/jmmorell/k8s-order-saga/internal/platform/testinfra"
)

func TestConnect_ReturnsContextErrorWhenNothingListens(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	nc, js, err := events.Connect(ctx, "nats://"+addr, "unreachable")
	elapsed := time.Since(start)

	require.Nil(t, nc)
	require.Nil(t, js)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.GreaterOrEqual(t, elapsed, 2*time.Second)
	require.Less(t, elapsed, 3*time.Second)
}

func TestConnect_SucceedsWhenTheServerBecomesReachableLater(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	url, proxy := testinfra.NATSBehindProxy(t)
	proxy.Down()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	type result struct {
		nc  *nats.Conn
		err error
	}
	done := make(chan result, 1)
	go func() {
		nc, _, err := events.Connect(ctx, url, "late")
		done <- result{nc, err}
	}()

	select {
	case r := <-done:
		t.Fatalf("Connect returned before the server was reachable: %v", r.err)
	case <-time.After(2 * time.Second):
	}
	proxy.Up()

	select {
	case r := <-done:
		require.NoError(t, r.err)
		t.Cleanup(r.nc.Close)
	case <-time.After(20 * time.Second):
		t.Fatal("Connect did not succeed after the server became reachable")
	}
}
