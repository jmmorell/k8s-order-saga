package notifications_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"k8s-portfolio/internal/notifications"
	"k8s-portfolio/internal/platform/events"
	"k8s-portfolio/internal/platform/testinfra"
)

type sent struct{ to, subject, body string }

type fakeSender struct {
	mu   sync.Mutex
	got  []sent
	fail error
}

func (f *fakeSender) Send(_ context.Context, to, subject, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	f.got = append(f.got, sent{to, subject, body})
	return nil
}

func newService(s notifications.Sender) *notifications.Service {
	return notifications.NewService(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestHandle_ConfirmedSendsEmail(t *testing.T) {
	f := &fakeSender{}
	id := uuid.New()
	e, err := events.New(events.OrderConfirmed, id,
		events.OrderConfirmedPayload{CustomerEmail: "a@b.c", AmountCents: 1250})
	require.NoError(t, err)

	require.NoError(t, newService(f).Handle(context.Background(), e))

	require.Len(t, f.got, 1)
	require.Equal(t, "a@b.c", f.got[0].to)
	require.Equal(t, fmt.Sprintf("Order %s confirmed", id), f.got[0].subject)
	require.Contains(t, f.got[0].body, "12.50")
}

func TestHandle_CancelledIncludesReason(t *testing.T) {
	f := &fakeSender{}
	id := uuid.New()
	e, err := events.New(events.OrderCancelled, id,
		events.OrderCancelledPayload{CustomerEmail: "a@b.c", Reason: events.ReasonOutOfStock})
	require.NoError(t, err)

	require.NoError(t, newService(f).Handle(context.Background(), e))

	require.Len(t, f.got, 1)
	require.Equal(t, fmt.Sprintf("Order %s cancelled (out_of_stock)", id), f.got[0].subject)
}

func TestHandle_SenderErrorIsRetryable(t *testing.T) {
	f := &fakeSender{fail: errors.New("smtp down")}
	e, err := events.New(events.OrderConfirmed, uuid.New(),
		events.OrderConfirmedPayload{CustomerEmail: "a@b.c"})
	require.NoError(t, err)

	err = newService(f).Handle(context.Background(), e)

	require.Error(t, err)
	require.NotErrorIs(t, err, events.ErrPermanent)
}

func TestHandle_UndecodablePayloadIsPermanent(t *testing.T) {
	f := &fakeSender{}
	e := events.Envelope{Type: events.OrderConfirmed, OrderID: uuid.New(), Payload: json.RawMessage(`"nope"`)}

	err := newService(f).Handle(context.Background(), e)

	require.ErrorIs(t, err, events.ErrPermanent)
	require.Empty(t, f.got)
}

func TestSMTPSender_DeliversToMailpit(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	smtpAddr, apiURL := testinfra.Mailpit(t)
	s := notifications.NewSMTPSender(smtpAddr, "shop@example.local")

	require.NoError(t, s.Send(context.Background(), "a@b.c", "Order X confirmed", "thanks"))

	require.Eventually(t, func() bool {
		resp, err := http.Get(apiURL + "/api/v1/messages")
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		var out struct {
			Total    int `json:"total"`
			Messages []struct {
				Subject string `json:"Subject"`
			} `json:"messages"`
		}
		if json.NewDecoder(resp.Body).Decode(&out) != nil || out.Total != 1 {
			return false
		}
		return out.Messages[0].Subject == "Order X confirmed"
	}, 10*time.Second, 100*time.Millisecond)
}

func TestRun_EmailsConfirmedOrder(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	natsURL := testinfra.NATS(t)
	smtpAddr, apiURL := testinfra.Mailpit(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- notifications.Run(ctx, notifications.Config{
			HTTPAddr: "127.0.0.1:0", NATSURL: natsURL, SMTPAddr: smtpAddr, From: "shop@example.local",
		}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()

	nc, js, err := events.Connect(natsURL, "test")
	require.NoError(t, err)
	defer nc.Close()
	// Run creates the streams; wait for them before publishing.
	require.Eventually(t, func() bool { return events.EnsureStreams(ctx, js) == nil }, 10*time.Second, 100*time.Millisecond)
	id := uuid.New()
	e, err := events.New(events.OrderConfirmed, id, events.OrderConfirmedPayload{CustomerEmail: "x@y.z", AmountCents: 500})
	require.NoError(t, err)
	require.NoError(t, events.Publish(ctx, js, e))

	require.Eventually(t, func() bool {
		resp, err := http.Get(apiURL + "/api/v1/messages")
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		var out struct{ Total int }
		return json.NewDecoder(resp.Body).Decode(&out) == nil && out.Total >= 1
	}, 15*time.Second, 200*time.Millisecond)

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}
