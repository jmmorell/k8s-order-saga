package orders

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const (
	stuckAfter           = "2 minutes"
	stuckQueryTimeout    = 2 * time.Second
	stuckPendingQuerySQL = `SELECT count(*) FROM orders WHERE status = 'PENDING' AND created_at < now() - interval '` + stuckAfter + `'`
)

// The instruments come from the global meter; the global provider hands them
// to the real provider once telemetry setup installs it.
var (
	meter = otel.Meter("github.com/jmmorell/k8s-order-saga/internal/orders")

	createdCounter   = mustCounter("saga.orders.created", "Orders accepted by POST /orders.")
	completedCounter = mustCounter("saga.orders.completed", "Orders that left PENDING, by final status and reason.")
	durationHist     = mustHistogram()
)

func mustCounter(name, desc string) metric.Int64Counter {
	c, err := meter.Int64Counter(name, metric.WithDescription(desc))
	if err != nil {
		panic(err) // only fails on an invalid instrument name
	}
	return c
}

func mustHistogram() metric.Float64Histogram {
	h, err := meter.Float64Histogram("saga.order.duration",
		metric.WithDescription("Time from order creation to its final state."),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60))
	if err != nil {
		panic(err)
	}
	return h
}

// recordCompleted counts an order that this call moved out of PENDING.
func recordCompleted(ctx context.Context, status Status, reason string, took time.Duration) {
	s := "confirmed"
	if status == StatusCancelled {
		s = "cancelled"
	}
	completedCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("status", s), attribute.String("reason", reason)))
	durationHist.Record(ctx, took.Seconds(), metric.WithAttributes(attribute.String("status", s)))
}

// RegisterMetrics registers the stuck-pending gauge, which counts orders still
// PENDING after two minutes. The returned function unregisters it. The
// callback never blocks the saga: the query has a 2 s timeout and on error the
// gauge observes nothing.
func (s *Service) RegisterMetrics() (unregister func() error, err error) {
	g, err := meter.Int64ObservableGauge("saga.orders.stuck_pending",
		metric.WithDescription("Orders PENDING for more than "+stuckAfter+"."))
	if err != nil {
		return nil, err
	}
	reg, err := meter.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		ctx, cancel := context.WithTimeout(ctx, stuckQueryTimeout)
		defer cancel()
		var n int64
		if err := s.pool.QueryRow(ctx, stuckPendingQuerySQL).Scan(&n); err != nil {
			s.log.WarnContext(ctx, "stuck pending orders query failed", "error", err)
			return nil
		}
		o.ObserveInt64(g, n)
		return nil
	}, g)
	if err != nil {
		return nil, err
	}
	return reg.Unregister, nil
}
