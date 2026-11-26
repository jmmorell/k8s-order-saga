// Package payments simulates charging an order once its stock is reserved.
package payments

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/jmmorell/k8s-order-saga/internal/platform/events"
	"github.com/jmmorell/k8s-order-saga/internal/platform/httpx"
	"github.com/jmmorell/k8s-order-saga/internal/platform/pgutil"
)

const (
	statusSucceeded = "SUCCEEDED"
	statusFailed    = "FAILED"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Migrations returns the service's embedded SQL migrations at the root of the FS.
func Migrations() fs.FS {
	sub, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		panic(err) // the path is a constant that exists at build time
	}
	return sub
}

// Config holds the settings of the payments service.
type Config struct {
	HTTPAddr    string
	DatabaseURL string
	NATSURL     string
	FailureRate float64       // probability in [0,1] that a charge is declined
	Latency     time.Duration // simulated time a charge takes
}

// Validate reports whether the simulation settings are usable.
func (c Config) Validate() error {
	if c.FailureRate < 0 || c.FailureRate > 1 {
		return fmt.Errorf("payments: failure rate %v is outside [0,1]", c.FailureRate)
	}
	if c.Latency < 0 {
		return fmt.Errorf("payments: latency %v is negative", c.Latency)
	}
	return nil
}

// Service holds the payments handlers.
type Service struct {
	pool *pgxpool.Pool
	js   jetstream.JetStream
	cfg  Config
	log  *slog.Logger
}

// NewService builds a Service on an already migrated pool.
func NewService(pool *pgxpool.Pool, js jetstream.JetStream, cfg Config, log *slog.Logger) *Service {
	return &Service{pool: pool, js: js, cfg: cfg, log: log}
}

type payment struct {
	ID          uuid.UUID
	AmountCents int64
	Status      string
}

// HandleInventoryReserved charges the order once and publishes the stored
// outcome. A redelivery keeps the first outcome and publishes it again.
func (s *Service) HandleInventoryReserved(ctx context.Context, e events.Envelope) error {
	var in events.InventoryReservedPayload
	if err := e.Decode(&in); err != nil {
		return err
	}

	// Simulated gateway latency, outside any transaction.
	if s.cfg.Latency > 0 {
		t := time.NewTimer(s.cfg.Latency)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}

	status, reason := statusSucceeded, ""
	if rand.Float64() < s.cfg.FailureRate {
		status, reason = statusFailed, events.ReasonDeclined
	}

	var (
		p       payment
		created bool // this call stored the payment
	)
	err := pgutil.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`INSERT INTO payments (id, order_id, amount_cents, status, reason)
			 VALUES ($1, $2, $3, $4, NULLIF($5, ''))
			 ON CONFLICT (order_id) DO NOTHING`,
			uuid.New(), e.OrderID, in.AmountCents, status, reason)
		if err != nil {
			return fmt.Errorf("insert payment: %w", err)
		}
		created = tag.RowsAffected() == 1
		if err := tx.QueryRow(ctx,
			`SELECT id, amount_cents, status FROM payments WHERE order_id = $1`, e.OrderID).
			Scan(&p.ID, &p.AmountCents, &p.Status); err != nil {
			return fmt.Errorf("read payment: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}

	// Count right after the commit: a failed publish is redelivered with
	// created=false, so counting after it would lose the payment.
	if created {
		result := "failed"
		if p.Status == statusSucceeded {
			result = "succeeded"
		}
		recordPayment(ctx, result)
	}
	if p.Status == statusSucceeded {
		if err := s.publish(ctx, events.PaymentSucceeded, e,
			events.PaymentSucceededPayload{PaymentID: p.ID, AmountCents: p.AmountCents}); err != nil {
			return err
		}
		if created {
			s.log.InfoContext(ctx, "payment succeeded", "order_id", e.OrderID.String(),
				"payment_id", p.ID.String(), "amount_cents", p.AmountCents)
		} else {
			s.logRepublished(ctx, e, events.PaymentSucceeded)
		}
		return nil
	}
	if err := s.publish(ctx, events.PaymentFailed, e, events.PaymentFailedPayload{Reason: events.ReasonDeclined}); err != nil {
		return err
	}
	if created {
		s.log.InfoContext(ctx, "payment failed", "order_id", e.OrderID.String(), "reason", events.ReasonDeclined)
	} else {
		s.logRepublished(ctx, e, events.PaymentFailed)
	}
	return nil
}

func (s *Service) logRepublished(ctx context.Context, e events.Envelope, subject events.Subject) {
	s.log.DebugContext(ctx, "re-published stored outcome", "order_id", e.OrderID.String(), "subject", string(subject))
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
	if err := cfg.Validate(); err != nil {
		return err
	}
	pool, err := pgutil.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := pgutil.Migrate(ctx, pool, Migrations()); err != nil {
		return err
	}

	nc, js, err := events.Connect(ctx, cfg.NATSURL, "payments")
	if err != nil {
		return err
	}
	defer nc.Close()
	if err := events.EnsureStreams(ctx, js); err != nil {
		return err
	}

	svc := NewService(pool, js, cfg, log)
	stop, err := events.Consume(ctx, js,
		events.ConsumerSpec{Stream: "INVENTORY", Durable: "payments", Subjects: []events.Subject{events.InventoryReserved}},
		svc.HandleInventoryReserved, log)
	if err != nil {
		return err
	}
	defer stop() // runs before nc.Close and pool.Close: cancelling ctx alone does not stop the subscription

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
