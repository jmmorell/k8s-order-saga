package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
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

	var e Envelope
	if err := json.Unmarshal(m.Data(), &e); err != nil {
		c.deadLetter(ctx, m, attempt, fmt.Errorf("invalid envelope: %w", err))
		return
	}

	err = c.h(ctx, e)
	switch {
	case err == nil:
		if err := m.Ack(); err != nil {
			c.log.Error("ack", "error", err, "event_id", e.ID)
		}
	case errors.Is(err, ErrPermanent), attempt >= c.spec.MaxDeliver:
		c.deadLetter(ctx, m, attempt, err)
	default:
		delay := c.spec.NakDelays[min(attempt-1, len(c.spec.NakDelays)-1)]
		c.log.Warn("handler failed, will retry", "error", err, "event_id", e.ID, "attempt", attempt, "delay", delay)
		if err := m.NakWithDelay(delay); err != nil {
			c.log.Error("nak", "error", err, "event_id", e.ID)
		}
	}
}

// deadLetter copies the original bytes to the DLQ and terminates the message.
// If the DLQ publish fails, the message is nak'd so it is not lost.
func (c *consumer) deadLetter(ctx context.Context, m jetstream.Msg, attempt int, cause error) {
	msg := &nats.Msg{
		Subject: "dlq." + c.spec.Durable + "." + m.Subject(),
		Data:    m.Data(),
		Header:  nats.Header{},
	}
	msg.Header.Set("X-Error", cause.Error())
	msg.Header.Set("X-Original-Subject", m.Subject())
	msg.Header.Set("X-Delivery-Count", strconv.Itoa(attempt))
	if _, err := c.js.PublishMsg(ctx, msg); err != nil {
		c.log.Error("publish to DLQ failed, will retry", "error", err, "subject", m.Subject(), "attempt", attempt)
		_ = m.NakWithDelay(c.spec.NakDelays[min(attempt-1, len(c.spec.NakDelays)-1)])
		return
	}
	c.log.Error("message sent to DLQ", "error", cause, "subject", m.Subject(), "attempt", attempt)
	if err := m.Term(); err != nil {
		c.log.Error("term", "error", err)
	}
}
