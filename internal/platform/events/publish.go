package events

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Publish stores the event in JetStream. Nats-Msg-Id is the event ID, so
// publishing the same event again within the duplicate window is a no-op.
func Publish(ctx context.Context, js jetstream.JetStream, e Envelope) (err error) {
	ctx, span := tracer().Start(ctx, "publish "+string(e.Type),
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(messagingAttrs(string(e.Type), e)...))
	defer func() {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}()

	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}
	msg := &nats.Msg{Subject: string(e.Type), Data: data, Header: nats.Header{}}
	msg.Header.Set(jetstream.MsgIDHeader, e.ID.String())
	otel.GetTextMapPropagator().Inject(ctx, headerCarrier(msg.Header))
	if _, err := js.PublishMsg(ctx, msg); err != nil {
		return fmt.Errorf("publish %s: %w", e.Type, err)
	}
	return nil
}
