// Package events is the shared messaging layer: event envelope, JetStream
// streams, publishing and consuming with retries and a dead-letter queue.
package events

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Subject is a NATS subject that doubles as the event type.
type Subject string

const (
	OrderCreated      Subject = "orders.created"
	OrderConfirmed    Subject = "orders.confirmed"
	OrderCancelled    Subject = "orders.cancelled"
	InventoryReserved Subject = "inventory.reserved"
	InventoryRejected Subject = "inventory.rejected"
	InventoryReleased Subject = "inventory.released"
	PaymentSucceeded  Subject = "payments.succeeded"
	PaymentFailed     Subject = "payments.failed"
)

// ErrPermanent marks an error that retrying cannot fix; the message goes
// straight to the DLQ.
var ErrPermanent = errors.New("permanent error")

// Envelope is the JSON wrapper around every event.
type Envelope struct {
	ID         uuid.UUID       `json:"event_id"`
	Type       Subject         `json:"type"`
	OccurredAt time.Time       `json:"occurred_at"`
	OrderID    uuid.UUID       `json:"order_id"`
	Payload    json.RawMessage `json:"payload"`
}

// New builds an envelope. The ID is derived from the order and subject, so
// republishing the same event yields the same ID and JetStream deduplicates it.
func New(subject Subject, orderID uuid.UUID, payload any) (Envelope, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return Envelope{}, fmt.Errorf("marshal payload: %w", err)
	}
	return Envelope{
		ID:         uuid.NewSHA1(orderID, []byte(subject)),
		Type:       subject,
		OccurredAt: time.Now().UTC(),
		OrderID:    orderID,
		Payload:    raw,
	}, nil
}

// Decode unmarshals the payload into v. Failures wrap ErrPermanent.
func (e Envelope) Decode(v any) error {
	if err := json.Unmarshal(e.Payload, v); err != nil {
		return fmt.Errorf("decode %s payload: %w: %w", e.Type, ErrPermanent, err)
	}
	return nil
}
