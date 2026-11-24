package events_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/jmmorell/k8s-order-saga/internal/platform/events"
	"github.com/jmmorell/k8s-order-saga/internal/platform/telemetry/telemetrytest"
)

var (
	recorder = tracetest.NewSpanRecorder()
	reader   *sdkmetric.ManualReader
)

func TestMain(m *testing.M) {
	reader = telemetrytest.NewReader()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)))
	otel.SetTextMapPropagator(propagation.TraceContext{})
	os.Exit(m.Run())
}

// endedSpan waits for an ended span with the given name and order.id.
func endedSpan(t *testing.T, name string, orderID uuid.UUID) sdktrace.ReadOnlySpan {
	t.Helper()
	var found sdktrace.ReadOnlySpan
	require.Eventually(t, func() bool {
		for _, s := range recorder.Ended() {
			if s.Name() != name {
				continue
			}
			for _, a := range s.Attributes() {
				if a.Key == "order.id" && a.Value.AsString() == orderID.String() {
					found = s
					return true
				}
			}
		}
		return false
	}, 5*time.Second, 20*time.Millisecond, "no ended span %q for order %s", name, orderID)
	return found
}

func TestTracing_ConsumerSpanIsChildOfProducerSpan(t *testing.T) {
	_, js := setup(t)
	done := make(chan struct{}, 1)
	startConsumer(t, js, events.ConsumerSpec{
		Stream: "ORDERS", Durable: "trace", Subjects: []events.Subject{events.OrderCreated},
	}, func(context.Context, events.Envelope) error { done <- struct{}{}; return nil })

	oid := uuid.New()
	ctx, parent := otel.Tracer("test").Start(context.Background(), "parent")
	e, err := events.New(events.OrderCreated, oid, events.OrderCreatedPayload{CustomerEmail: "a@b.c"})
	require.NoError(t, err)
	require.NoError(t, events.Publish(ctx, js, e))
	parent.End()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("handler not called")
	}

	pub := endedSpan(t, "publish orders.created", oid)
	proc := endedSpan(t, "process orders.created", oid)
	require.Equal(t, trace.SpanKindProducer, pub.SpanKind())
	require.Equal(t, trace.SpanKindConsumer, proc.SpanKind())
	require.Equal(t, parent.SpanContext().TraceID(), pub.SpanContext().TraceID())
	require.Equal(t, parent.SpanContext().TraceID(), proc.SpanContext().TraceID())
	require.Equal(t, pub.SpanContext().SpanID(), proc.Parent().SpanID())
	require.Contains(t, proc.Attributes(), attribute.String("messaging.consumer.group.name", "trace"))
	require.Contains(t, proc.Attributes(), attribute.String("messaging.system", "nats"))
	require.Contains(t, pub.Attributes(), attribute.String("messaging.message.id", e.ID.String()))
}

func TestTracing_MessageWithoutTraceparentStartsRootSpan(t *testing.T) {
	_, js := setup(t)
	calls := make(chan struct{}, 2)
	startConsumer(t, js, events.ConsumerSpec{
		Stream: "ORDERS", Durable: "trace", Subjects: []events.Subject{events.OrderCreated},
	}, func(context.Context, events.Envelope) error { calls <- struct{}{}; return nil })

	oid := uuid.New()
	e, err := events.New(events.OrderCreated, oid, events.OrderCreatedPayload{CustomerEmail: "a@b.c"})
	require.NoError(t, err)
	data, err := json.Marshal(e)
	require.NoError(t, err)
	_, err = js.PublishMsg(context.Background(), &nats.Msg{Subject: string(e.Type), Data: data})
	require.NoError(t, err)

	select {
	case <-calls:
	case <-time.After(10 * time.Second):
		t.Fatal("handler not called")
	}
	proc := endedSpan(t, "process orders.created", oid)
	require.False(t, proc.Parent().IsValid())
	require.Len(t, calls, 0)
	cons, err := js.Consumer(context.Background(), "ORDERS", "trace")
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		ci, err := cons.Info(context.Background())
		return err == nil && ci.NumAckPending == 0 && ci.AckFloor.Consumer == 1
	}, 5*time.Second, 50*time.Millisecond)
}

func TestTracing_DeadLetterKeepsTraceparent(t *testing.T) {
	_, js := setup(t)
	dlq := dlqMessages(t, js, "dlq.trace.orders.created")
	startConsumer(t, js, events.ConsumerSpec{
		Stream: "ORDERS", Durable: "trace", Subjects: []events.Subject{events.OrderCreated},
	}, func(context.Context, events.Envelope) error { return events.ErrPermanent })

	const tp = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	e, err := events.New(events.OrderCreated, uuid.New(), events.OrderCreatedPayload{CustomerEmail: "a@b.c"})
	require.NoError(t, err)
	data, err := json.Marshal(e)
	require.NoError(t, err)
	msg := &nats.Msg{Subject: string(e.Type), Data: data, Header: nats.Header{}}
	msg.Header.Set("traceparent", tp)
	_, err = js.PublishMsg(context.Background(), msg)
	require.NoError(t, err)

	select {
	case m := <-dlq:
		require.Equal(t, tp, m.Headers().Get("traceparent"))
	case <-time.After(10 * time.Second):
		t.Fatal("no DLQ message")
	}
}
