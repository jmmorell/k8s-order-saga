// Package orders exposes the HTTP API that creates orders and closes them when the saga result arrives.
package orders

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/jmmorell/k8s-order-saga/internal/platform/events"
	"github.com/jmmorell/k8s-order-saga/internal/platform/httpx"
	"github.com/jmmorell/k8s-order-saga/internal/platform/pgutil"
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

// Status is the state of an order.
type Status string

const (
	StatusPending   Status = "PENDING"
	StatusConfirmed Status = "CONFIRMED"
	StatusCancelled Status = "CANCELLED"
)

// Order is an order as returned by the API.
type Order struct {
	ID            uuid.UUID     `json:"id"`
	Status        Status        `json:"status"`
	Reason        string        `json:"reason,omitempty"`
	CustomerEmail string        `json:"customer_email"`
	Items         []events.Item `json:"items"`
	AmountCents   *int64        `json:"amount_cents,omitempty"`
	CreatedAt     time.Time     `json:"created_at"`
	UpdatedAt     time.Time     `json:"updated_at"`
}

// Config holds the settings of the orders service.
type Config struct {
	HTTPAddr    string
	DatabaseURL string
	NATSURL     string
}

// Service holds the orders handlers.
type Service struct {
	pool *pgxpool.Pool
	js   jetstream.JetStream
	log  *slog.Logger
}

// NewService builds a Service on an already migrated pool.
func NewService(pool *pgxpool.Pool, js jetstream.JetStream, log *slog.Logger) *Service {
	return &Service{pool: pool, js: js, log: log}
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

	nc, js, err := events.Connect(ctx, cfg.NATSURL, "orders")
	if err != nil {
		return err
	}
	defer nc.Close()
	if err := events.EnsureStreams(ctx, js); err != nil {
		return err
	}

	svc := NewService(pool, js, log)
	unregister, err := svc.RegisterMetrics()
	if err != nil {
		return err
	}
	defer func() { _ = unregister() }()
	for _, spec := range []events.ConsumerSpec{
		{Stream: "INVENTORY", Durable: "orders", Subjects: []events.Subject{events.InventoryRejected}},
		{Stream: "PAYMENTS", Durable: "orders", Subjects: []events.Subject{events.PaymentSucceeded, events.PaymentFailed}},
	} {
		stop, err := events.Consume(ctx, js, spec, svc.HandleResult, log)
		if err != nil {
			return err
		}
		defer stop() // runs before nc.Close and pool.Close: cancelling ctx alone does not stop the subscription
	}

	health := httpx.HealthMux(map[string]httpx.ReadyCheck{
		"db": func(ctx context.Context) error { return pool.Ping(ctx) },
		"nats": func(context.Context) error {
			if !nc.IsConnected() {
				return fmt.Errorf("nats is %s", nc.Status())
			}
			return nil
		},
	})
	root := newRootMux(health, svc.Routes())
	return httpx.Serve(ctx, cfg.HTTPAddr, root, log)
}
