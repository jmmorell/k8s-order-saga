package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const (
	defaultMaxDeliver = 5
	ackWait           = 30 * time.Second
)

var defaultNakDelays = []time.Duration{time.Second, 5 * time.Second, 15 * time.Second, 30 * time.Second}

// Handler processes one event. Returning nil acknowledges it, an error
// wrapping ErrPermanent sends it to the DLQ, any other error retries it.
type Handler func(ctx context.Context, e Envelope) error

// ConsumerSpec describes a durable consumer on one stream.
type ConsumerSpec struct {
	Stream     string
	Durable    string
	Subjects   []Subject
	MaxDeliver int             // default 5
	NakDelays  []time.Duration // default 1s, 5s, 15s, 30s
}

// Consume starts a durable pull consumer and dispatches messages to h.
// After MaxDeliver failed attempts, or on a permanent error or an invalid
// envelope, the message is copied to dlq.<durable>.<subject> and terminated.
// The returned stop function ends consumption.
func Consume(ctx context.Context, js jetstream.JetStream, spec ConsumerSpec, h Handler, log *slog.Logger) (func(), error) {
	if spec.MaxDeliver <= 0 {
		spec.MaxDeliver = defaultMaxDeliver
	}
	if len(spec.NakDelays) == 0 {
		spec.NakDelays = defaultNakDelays
	}
	filters := make([]string, len(spec.Subjects))
	for i, s := range spec.Subjects {
		filters[i] = string(s)
	}
	cfg := jetstream.ConsumerConfig{
		Durable:       spec.Durable,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       ackWait,
		DeliverPolicy: jetstream.DeliverAllPolicy,
		// MaxDeliver is enforced client-side only: a server-side limit would stop
		// redelivery after the last attempt, losing a message whose DLQ publish failed.
		MaxDeliver:     -1,
		FilterSubjects: filters,
	}
	if len(filters) == 1 {
		cfg.FilterSubject, cfg.FilterSubjects = filters[0], nil
	}
	cons, err := js.CreateOrUpdateConsumer(ctx, spec.Stream, cfg)
	if err != nil {
		return nil, fmt.Errorf("create consumer %s on %s: %w", spec.Durable, spec.Stream, err)
	}

	c := &consumer{js: js, spec: spec, h: h, log: log.With("durable", spec.Durable)}
	cc, err := cons.Consume(func(m jetstream.Msg) { c.handle(ctx, m) })
	if err != nil {
		return nil, fmt.Errorf("start consuming %s: %w", spec.Durable, err)
	}
	return cc.Stop, nil
}

type consumer struct {
	js   jetstream.JetStream
	spec ConsumerSpec
	h    Handler
	log  *slog.Logger
}

func (c *consumer) handle(ctx context.Context, m jetstream.Msg) {
	meta, err := m.Metadata()
	if err != nil {
		c.log.Error("read message metadata", "error", err)
		_ = m.Nak()
		return
	}
	attempt := int(meta.NumDelivered)

	// A message without trace context starts a new root trace.
	ctx = otel.GetTextMapPropagator().Extract(ctx, headerCarrier(m.Headers()))
	ctx, span := tracer().Start(ctx, "process "+m.Subject(),
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			attribute.String("messaging.system", "nats"),
			attribute.String("messaging.destination.name", m.Subject()),
			attribute.String("messaging.consumer.group.name", c.spec.Durable),
			attribute.Int("messaging.nats.delivery_attempt", attempt),
		))
	defer span.End()

	var e Envelope
	if err := json.Unmarshal(m.Data(), &e); err != nil {
		err = fmt.Errorf("invalid envelope: %w", err)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		c.deadLetter(ctx, m, attempt, err)
		return
	}
	span.SetAttributes(
		attribute.String("messaging.message.id", e.ID.String()),
		attribute.String("order.id", e.OrderID.String()),
	)

	err = c.call(ctx, e)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	switch {
	case err == nil:
		recordConsumed(ctx, c.spec.Durable, m.Subject(), resultAck)
		if err := m.Ack(); err != nil {
			c.log.Error("ack", "error", err, "event_id", e.ID)
		}
	case errors.Is(err, ErrPermanent), attempt >= c.spec.MaxDeliver:
		c.deadLetter(ctx, m, attempt, err)
	default:
		delay := c.spec.NakDelays[min(attempt-1, len(c.spec.NakDelays)-1)]
		recordConsumed(ctx, c.spec.Durable, m.Subject(), resultRetry)
		c.log.Warn("handler failed, will retry", "error", err, "event_id", e.ID, "attempt", attempt, "delay", delay)
		if err := m.NakWithDelay(delay); err != nil {
			c.log.Error("nak", "error", err, "event_id", e.ID)
		}
	}
}

// call runs the handler and turns a panic into a retryable error, so one bad
// message cannot crash the service or block the consumer.
func (c *consumer) call(ctx context.Context, e Envelope) (err error) {
	defer func() {
		if r := recover(); r != nil {
			c.log.Error("handler panicked", "panic", r, "order_id", e.OrderID, "event_id", e.ID, "stack", string(debug.Stack()))
			err = fmt.Errorf("handler panicked: %v", r)
		}
	}()
	return c.h(ctx, e)
}

// deadLetter copies the original bytes to the DLQ and terminates the message.
// If the DLQ publish fails, the message is nak'd so it is not lost.
func (c *consumer) deadLetter(ctx context.Context, m jetstream.Msg, attempt int, cause error) {
	msg := &nats.Msg{
		Subject: "dlq." + c.spec.Durable + "." + m.Subject(),
		Data:    m.Data(),
		Header:  nats.Header{},
	}
	for _, k := range []string{"traceparent", "tracestate"} {
		if v := m.Headers().Get(k); v != "" {
			msg.Header.Set(k, v)
		}
	}
	msg.Header.Set("X-Error", cause.Error())
	msg.Header.Set("X-Original-Subject", m.Subject())
	msg.Header.Set("X-Delivery-Count", strconv.Itoa(attempt))
	if _, err := c.js.PublishMsg(ctx, msg); err != nil {
		c.log.Error("publish to DLQ failed, will retry", "error", err, "subject", m.Subject(), "attempt", attempt)
		recordConsumed(ctx, c.spec.Durable, m.Subject(), resultRetry)
		_ = m.NakWithDelay(c.spec.NakDelays[min(attempt-1, len(c.spec.NakDelays)-1)])
		return
	}
	recordConsumed(ctx, c.spec.Durable, m.Subject(), resultDeadLetter)
	c.log.Error("message sent to DLQ", "error", cause, "subject", m.Subject(), "attempt", attempt)
	if err := m.Term(); err != nil {
		c.log.Error("term", "error", err)
	}
}
