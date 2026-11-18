package events_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/jmmorell/k8s-order-saga/internal/platform/events"
	"github.com/jmmorell/k8s-order-saga/internal/platform/testinfra"
)

func setup(t *testing.T) (*nats.Conn, jetstream.JetStream) {
	t.Helper()
	if testing.Short() {
		t.Skip()
	}
	nc, js, err := events.Connect(testinfra.NATS(t), "test")
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, events.EnsureStreams(ctx, js))
	return nc, js
}

func publishCreated(t *testing.T, js jetstream.JetStream, orderID uuid.UUID) {
	t.Helper()
	e, err := events.New(events.OrderCreated, orderID, events.OrderCreatedPayload{CustomerEmail: "a@b.c"})
	require.NoError(t, err)
	require.NoError(t, events.Publish(context.Background(), js, e))
}

func startConsumer(t *testing.T, js jetstream.JetStream, spec events.ConsumerSpec, h events.Handler) {
	t.Helper()
	stop, err := events.Consume(context.Background(), js, spec, h, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	t.Cleanup(stop)
}

// dlqMessages subscribes to the DLQ stream and returns a channel of messages for the subject.
func dlqMessages(t *testing.T, js jetstream.JetStream, subject string) <-chan jetstream.Msg {
	t.Helper()
	ctx := context.Background()
	cons, err := js.CreateOrUpdateConsumer(ctx, "DLQ", jetstream.ConsumerConfig{FilterSubject: subject})
	require.NoError(t, err)
	out := make(chan jetstream.Msg, 10)
	cc, err := cons.Consume(func(m jetstream.Msg) { _ = m.Ack(); out <- m })
	require.NoError(t, err)
	t.Cleanup(cc.Stop)
	return out
}

func TestPublish_SameEventTwiceIsStoredOnce(t *testing.T) {
	_, js := setup(t)
	oid := uuid.New()
	publishCreated(t, js, oid)
	publishCreated(t, js, oid)

	s, err := js.Stream(context.Background(), "ORDERS")
	require.NoError(t, err)
	info, err := s.Info(context.Background())
	require.NoError(t, err)
	require.EqualValues(t, 1, info.State.Msgs)
}

func TestConsume_AckOnSuccess(t *testing.T) {
	_, js := setup(t)
	got := make(chan events.Envelope, 1)
	startConsumer(t, js, events.ConsumerSpec{
		Stream: "ORDERS", Durable: "test", Subjects: []events.Subject{events.OrderCreated},
	}, func(_ context.Context, e events.Envelope) error { got <- e; return nil })

	oid := uuid.New()
	publishCreated(t, js, oid)
	select {
	case e := <-got:
		require.Equal(t, oid, e.OrderID)
	case <-time.After(10 * time.Second):
		t.Fatal("handler not called")
	}

	cons, err := js.Consumer(context.Background(), "ORDERS", "test")
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		ci, err := cons.Info(context.Background())
		return err == nil && ci.NumAckPending == 0 && ci.AckFloor.Consumer == 1
	}, 5*time.Second, 50*time.Millisecond)
}

func TestConsume_PoisonGoesToDLQAndDoesNotBlock(t *testing.T) {
	_, js := setup(t)
	dlq := dlqMessages(t, js, "dlq.test.orders.created")
	orderA, orderB := uuid.New(), uuid.New()

	var mu sync.Mutex
	handled := map[uuid.UUID]bool{}
	handledB := make(chan struct{})
	startConsumer(t, js, events.ConsumerSpec{
		Stream: "ORDERS", Durable: "test", Subjects: []events.Subject{events.OrderCreated},
		MaxDeliver: 3, NakDelays: []time.Duration{50 * time.Millisecond},
	}, func(_ context.Context, e events.Envelope) error {
		if e.OrderID == orderA {
			return errors.New("boom")
		}
		mu.Lock()
		defer mu.Unlock()
		if !handled[e.OrderID] {
			handled[e.OrderID] = true
			close(handledB)
		}
		return nil
	})

	publishCreated(t, js, orderA)
	publishCreated(t, js, orderB)

	select {
	case <-handledB:
	case <-time.After(10 * time.Second):
		t.Fatal("order B was blocked by the poison message")
	}
	select {
	case m := <-dlq:
		require.Equal(t, "3", m.Headers().Get("X-Delivery-Count"))
		require.Equal(t, "orders.created", m.Headers().Get("X-Original-Subject"))
		require.Contains(t, m.Headers().Get("X-Error"), "boom")
		require.Contains(t, string(m.Data()), orderA.String())
	case <-time.After(10 * time.Second):
		t.Fatal("no DLQ message")
	}
}

func TestConsume_PermanentErrorGoesToDLQOnFirstDelivery(t *testing.T) {
	_, js := setup(t)
	dlq := dlqMessages(t, js, "dlq.test.orders.created")
	startConsumer(t, js, events.ConsumerSpec{
		Stream: "ORDERS", Durable: "test", Subjects: []events.Subject{events.OrderCreated},
	}, func(context.Context, events.Envelope) error {
		return fmt.Errorf("bad input: %w", events.ErrPermanent)
	})
	publishCreated(t, js, uuid.New())

	select {
	case m := <-dlq:
		require.Equal(t, "1", m.Headers().Get("X-Delivery-Count"))
	case <-time.After(10 * time.Second):
		t.Fatal("no DLQ message")
	}
}

func TestConsume_InvalidJSONGoesToDLQ(t *testing.T) {
	nc, js := setup(t)
	dlq := dlqMessages(t, js, "dlq.test.orders.created")
	var called atomic.Bool
	startConsumer(t, js, events.ConsumerSpec{
		Stream: "ORDERS", Durable: "test", Subjects: []events.Subject{events.OrderCreated},
	}, func(context.Context, events.Envelope) error { called.Store(true); return nil })

	_, err := js.Publish(context.Background(), string(events.OrderCreated), []byte("not-json"))
	require.NoError(t, err)
	_ = nc

	select {
	case m := <-dlq:
		require.Equal(t, "not-json", string(m.Data()))
	case <-time.After(10 * time.Second):
		t.Fatal("no DLQ message")
	}
	require.False(t, called.Load())
}

func TestConsume_FailedDLQPublishIsRetriedNotLost(t *testing.T) {
	_, js := setup(t)
	ctx := context.Background()
	require.NoError(t, js.DeleteStream(ctx, "DLQ"))

	var calls atomic.Int32
	startConsumer(t, js, events.ConsumerSpec{
		Stream: "ORDERS", Durable: "test", Subjects: []events.Subject{events.OrderCreated},
		MaxDeliver: 1, NakDelays: []time.Duration{50 * time.Millisecond},
	}, func(context.Context, events.Envelope) error {
		calls.Add(1)
		return fmt.Errorf("bad input: %w", events.ErrPermanent)
	})
	publishCreated(t, js, uuid.New())

	// The DLQ stream is gone, so dead-lettering fails and the message is redelivered.
	require.Eventually(t, func() bool { return calls.Load() >= 2 }, 10*time.Second, 20*time.Millisecond)

	require.NoError(t, events.EnsureStreams(ctx, js))
	dlq := dlqMessages(t, js, "dlq.test.orders.created")
	select {
	case m := <-dlq:
		require.Equal(t, "orders.created", m.Headers().Get("X-Original-Subject"))
	case <-time.After(10 * time.Second):
		t.Fatal("message was lost after DLQ publish failure")
	}
}
