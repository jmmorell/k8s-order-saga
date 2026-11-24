package orders

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jmmorell/k8s-order-saga/internal/platform/events"
	"github.com/jmmorell/k8s-order-saga/internal/platform/pgutil"
)

// HandleResult closes a pending order from an inventory or payment result,
// then publishes the stored outcome. A redelivery or a late result for an
// already closed order keeps the first outcome and publishes it again.
func (s *Service) HandleResult(ctx context.Context, e events.Envelope) error {
	var (
		status Status
		reason string
		email  string
		amount *int64
		closed bool // this call moved the order out of PENDING
		took   time.Duration
	)
	err := pgutil.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		var stored *string
		err := tx.QueryRow(ctx,
			`SELECT status, reason, customer_email, amount_cents FROM orders WHERE id = $1 FOR UPDATE`, e.OrderID).
			Scan(&status, &stored, &email, &amount)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("order %s not found: %w", e.OrderID, events.ErrPermanent)
		}
		if err != nil {
			return fmt.Errorf("read order: %w", err)
		}
		if stored != nil {
			reason = *stored
		}
		if status != StatusPending {
			return nil
		}

		switch e.Type {
		case events.InventoryRejected:
			var p events.InventoryRejectedPayload
			if err := e.Decode(&p); err != nil {
				return err
			}
			status, reason = StatusCancelled, p.Reason
		case events.PaymentFailed:
			status, reason = StatusCancelled, events.ReasonPaymentFailed
		case events.PaymentSucceeded:
			var p events.PaymentSucceededPayload
			if err := e.Decode(&p); err != nil {
				return err
			}
			status, amount = StatusConfirmed, &p.AmountCents
		default:
			return fmt.Errorf("unexpected event %s: %w", e.Type, events.ErrPermanent)
		}
		var seconds float64 // computed by the database: now() is not subject to clock skew between pods
		if err := tx.QueryRow(ctx,
			`UPDATE orders SET status = $2, reason = NULLIF($3, ''), amount_cents = $4, updated_at = now() WHERE id = $1
			 RETURNING extract(epoch FROM now() - created_at)::float8`,
			e.OrderID, status, reason, amount).Scan(&seconds); err != nil {
			return fmt.Errorf("update order: %w", err)
		}
		took = time.Duration(seconds * float64(time.Second))
		closed = true
		return nil
	})
	if err != nil {
		return err
	}
	if closed {
		recordCompleted(ctx, status, reason, took)
	}

	switch status {
	case StatusConfirmed:
		if err := s.publish(ctx, events.OrderConfirmed, e,
			events.OrderConfirmedPayload{CustomerEmail: email, AmountCents: *amount}); err != nil {
			return err
		}
		if closed {
			s.log.InfoContext(ctx, "order confirmed", "order_id", e.OrderID.String(), "amount_cents", *amount)
		} else {
			s.logRepublished(ctx, e, events.OrderConfirmed)
		}
	case StatusCancelled:
		if err := s.publish(ctx, events.OrderCancelled, e,
			events.OrderCancelledPayload{CustomerEmail: email, Reason: reason}); err != nil {
			return err
		}
		if closed {
			s.log.InfoContext(ctx, "order cancelled", "order_id", e.OrderID.String(), "reason", reason)
		} else {
			s.logRepublished(ctx, e, events.OrderCancelled)
		}
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
