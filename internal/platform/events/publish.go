package events

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Publish stores the event in JetStream. Nats-Msg-Id is the event ID, so
// publishing the same event again within the duplicate window is a no-op.
func Publish(ctx context.Context, js jetstream.JetStream, e Envelope) error {
	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}
	msg := &nats.Msg{Subject: string(e.Type), Data: data, Header: nats.Header{}}
	msg.Header.Set(jetstream.MsgIDHeader, e.ID.String())
	if _, err := js.PublishMsg(ctx, msg); err != nil {
		return fmt.Errorf("publish %s: %w", e.Type, err)
	}
	return nil
}
