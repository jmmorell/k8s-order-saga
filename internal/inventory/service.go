// Package inventory reserves, rejects and releases stock for orders.
package inventory

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/jmmorell/k8s-order-saga/internal/platform/events"
	"github.com/jmmorell/k8s-order-saga/internal/platform/httpx"
	"github.com/jmmorell/k8s-order-saga/internal/platform/pgutil"
)

// Config holds the settings of the inventory service.
type Config struct {
	HTTPAddr        string
	DatabaseURL     string
	NATSURL         string
	RestockInterval time.Duration
}

// Service holds the inventory handlers.
type Service struct {
	pool *pgxpool.Pool
	js   jetstream.JetStream
	log  *slog.Logger
}

// NewService builds a Service on an already migrated pool.
func NewService(pool *pgxpool.Pool, js jetstream.JetStream, log *slog.Logger) *Service {
	return &Service{pool: pool, js: js, log: log}
}

// HandleOrderCreated reserves stock for the order, or rejects it, and
// publishes the stored outcome. A redelivery publishes the stored outcome again.
func (s *Service) HandleOrderCreated(ctx context.Context, e events.Envelope) error {
	var p events.OrderCreatedPayload
	if err := e.Decode(&p); err != nil {
		return err
	}
	if len(p.Items) == 0 {
		return fmt.Errorf("order %s has no items: %w", e.OrderID, events.ErrPermanent)
	}
	for _, it := range p.Items {
		if it.Qty <= 0 {
			return fmt.Errorf("order %s has non-positive quantity for %s: %w", e.OrderID, it.SKU, events.ErrPermanent)
		}
	}

	var r reservation
	err := pgutil.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		stored, found, err := getReservation(ctx, tx, e.OrderID)
		if err != nil {
			return err
		}
		if found {
			r = stored
			return nil
		}
		if r, err = reserve(ctx, tx, e.OrderID, p.Items); err != nil {
			return err
		}
		return insertReservation(ctx, tx, r)
	})
	if err != nil {
		return err
	}

	switch r.Status {
	case statusReserved:
		return s.publish(ctx, events.InventoryReserved, e, events.InventoryReservedPayload{Items: r.Items, AmountCents: r.AmountCents})
	case statusRejected:
		return s.publish(ctx, events.InventoryRejected, e, events.InventoryRejectedPayload{Reason: r.Reason, SKU: r.SKU})
	}
	return nil // RELEASED: the order was already compensated, nothing to announce
}

// HandlePaymentFailed releases the stock reserved for the order and publishes
// inventory.released. Releasing twice is a no-op that publishes the event again.
func (s *Service) HandlePaymentFailed(ctx context.Context, e events.Envelope) error {
	var status string
	err := pgutil.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		r, found, err := getReservation(ctx, tx, e.OrderID)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("no reservation for order %s: %w", e.OrderID, events.ErrPermanent)
		}
		status = r.Status
		if r.Status != statusReserved {
			return nil
		}
		if err := release(ctx, tx, r.Items); err != nil {
			return err
		}
		status = statusReleased
		return setStatus(ctx, tx, e.OrderID, statusReleased)
	})
	if err != nil {
		return err
	}
	if status == statusReleased {
		return s.publish(ctx, events.InventoryReleased, e, events.InventoryReleasedPayload{})
	}
	return nil
}

// Restock resets every product's stock to its restock_to level.
func (s *Service) Restock(ctx context.Context) error {
	return restock(ctx, s.pool)
}

func (s *Service) publish(ctx context.Context, subject events.Subject, cause events.Envelope, payload any) error {
	out, err := events.New(subject, cause.OrderID, payload)
	if err != nil {
		return err
	}
	return events.Publish(ctx, s.js, out)
}

// Run starts the service and blocks until ctx ends.
func Run(ctx context.Context, cfg Config, log *slog.Logger) error {
	pool, err := pgutil.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := pgutil.Migrate(ctx, pool, Migrations()); err != nil {
		return err
	}

	nc, js, err := events.Connect(cfg.NATSURL, "inventory")
	if err != nil {
		return err
	}
	defer nc.Close()
	if err := events.EnsureStreams(ctx, js); err != nil {
		return err
	}

	svc := NewService(pool, js, log)
	var stops []func()
	defer func() {
		for _, stop := range stops {
			stop()
		}
	}()
	for _, c := range []struct {
		spec events.ConsumerSpec
		h    events.Handler
	}{
		{events.ConsumerSpec{Stream: "ORDERS", Durable: "inventory", Subjects: []events.Subject{events.OrderCreated}}, svc.HandleOrderCreated},
		{events.ConsumerSpec{Stream: "PAYMENTS", Durable: "inventory", Subjects: []events.Subject{events.PaymentFailed}}, svc.HandlePaymentFailed},
	} {
		stop, err := events.Consume(ctx, js, c.spec, c.h, log)
		if err != nil {
			return err
		}
		stops = append(stops, stop)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.restockLoop(ctx, cfg.RestockInterval)
	}()
	defer func() { cancel(); <-done }()

	mux := httpx.HealthMux(map[string]httpx.ReadyCheck{
		"db": func(ctx context.Context) error { return pool.Ping(ctx) },
		"nats": func(context.Context) error {
			if !nc.IsConnected() {
				return fmt.Errorf("nats is %s", nc.Status())
			}
			return nil
		},
	})
	return httpx.Serve(ctx, cfg.HTTPAddr, mux, log)
}

func (s *Service) restockLoop(ctx context.Context, every time.Duration) {
	if every <= 0 {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.Restock(ctx); err != nil && ctx.Err() == nil {
				s.log.Error("restock failed", "error", err)
			}
		}
	}
}
