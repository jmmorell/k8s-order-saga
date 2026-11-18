// Package notifications emails the final result of each order.
//
// There is no database: delivery is at-least-once, so a redelivered event can
// send a duplicate email.
package notifications

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jmmorell/k8s-order-saga/internal/platform/events"
	"github.com/jmmorell/k8s-order-saga/internal/platform/httpx"
)

// Config holds the settings of the notifications service.
type Config struct {
	HTTPAddr string
	NATSURL  string
	SMTPAddr string
	From     string
}

// Service turns order outcome events into emails.
type Service struct {
	sender Sender
	log    *slog.Logger
}

// NewService builds a Service that sends through s.
func NewService(s Sender, log *slog.Logger) *Service {
	return &Service{sender: s, log: log}
}

// Handle emails the outcome carried by an orders.confirmed or orders.cancelled
// event. An undecodable payload is permanent; a send failure is retried.
func (s *Service) Handle(ctx context.Context, e events.Envelope) error {
	var to, subject, body string
	switch e.Type {
	case events.OrderConfirmed:
		var p events.OrderConfirmedPayload
		if err := e.Decode(&p); err != nil {
			return err
		}
		to = p.CustomerEmail
		subject = fmt.Sprintf("Order %s confirmed", e.OrderID)
		body = fmt.Sprintf("Your order %s is confirmed. Total: %d.%02d.\n",
			e.OrderID, p.AmountCents/100, p.AmountCents%100)
	case events.OrderCancelled:
		var p events.OrderCancelledPayload
		if err := e.Decode(&p); err != nil {
			return err
		}
		to = p.CustomerEmail
		subject = fmt.Sprintf("Order %s cancelled (%s)", e.OrderID, p.Reason)
		body = fmt.Sprintf("Your order %s was cancelled. Reason: %s.\n", e.OrderID, p.Reason)
	default:
		return fmt.Errorf("notifications: unexpected event type %q: %w", e.Type, events.ErrPermanent)
	}

	if err := s.sender.Send(ctx, to, subject, body); err != nil {
		return fmt.Errorf("send email for order %s: %w", e.OrderID, err)
	}
	s.log.Info("email sent", "order_id", e.OrderID, "subject", subject)
	return nil
}

// Run starts the service and blocks until ctx ends.
func Run(ctx context.Context, cfg Config, log *slog.Logger) error {
	nc, js, err := events.Connect(cfg.NATSURL, "notifications")
	if err != nil {
		return err
	}
	defer nc.Close()
	if err := events.EnsureStreams(ctx, js); err != nil {
		return err
	}

	svc := NewService(NewSMTPSender(cfg.SMTPAddr, cfg.From), log)
	stop, err := events.Consume(ctx, js,
		events.ConsumerSpec{Stream: "ORDERS", Durable: "notifications",
			Subjects: []events.Subject{events.OrderConfirmed, events.OrderCancelled}},
		svc.Handle, log)
	if err != nil {
		return err
	}
	defer stop() // runs before nc.Close: cancelling ctx alone does not stop the subscription

	mux := httpx.HealthMux(map[string]httpx.ReadyCheck{
		"nats": func(context.Context) error {
			if !nc.IsConnected() {
				return fmt.Errorf("nats is %s", nc.Status())
			}
			return nil
		},
	})
	return httpx.Serve(ctx, cfg.HTTPAddr, mux, log)
}
