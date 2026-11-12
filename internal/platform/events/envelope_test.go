package events_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"k8s-portfolio/internal/platform/events"
)

func TestNew_IDIsDeterministicPerOrderAndSubject(t *testing.T) {
	oid := uuid.New()
	a, err := events.New(events.OrderCreated, oid, events.OrderCreatedPayload{})
	require.NoError(t, err)
	b, err := events.New(events.OrderCreated, oid, events.OrderCreatedPayload{})
	require.NoError(t, err)
	c, err := events.New(events.OrderConfirmed, oid, events.OrderConfirmedPayload{})
	require.NoError(t, err)
	require.Equal(t, a.ID, b.ID)
	require.NotEqual(t, a.ID, c.ID)
}

func TestEnvelope_RoundTrip(t *testing.T) {
	oid := uuid.New()
	e, err := events.New(events.OrderCreated, oid, events.OrderCreatedPayload{
		CustomerEmail: "a@b.c",
		Items:         []events.Item{{SKU: "SKU-001", Qty: 2}},
	})
	require.NoError(t, err)
	b, err := json.Marshal(e)
	require.NoError(t, err)
	require.Contains(t, string(b), `"type":"orders.created"`)
	require.Contains(t, string(b), `"customer_email":"a@b.c"`)

	var back events.Envelope
	require.NoError(t, json.Unmarshal(b, &back))
	require.Equal(t, oid, back.OrderID)
	var p events.OrderCreatedPayload
	require.NoError(t, back.Decode(&p))
	require.Equal(t, 2, p.Items[0].Qty)
}

func TestDecode_BadPayloadIsPermanent(t *testing.T) {
	e := events.Envelope{Payload: json.RawMessage(`"x"`)}
	var p events.OrderCreatedPayload
	err := e.Decode(&p)
	require.Error(t, err)
	require.True(t, errors.Is(err, events.ErrPermanent))
}
