package orders

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/jmmorell/k8s-order-saga/internal/platform/events"
)

const (
	maxBodyBytes   = 64 << 10
	maxItems       = 20
	maxQty         = 100
	publishTimeout = 5 * time.Second
)

// CreateOrderRequest is the body of POST /orders.
type CreateOrderRequest struct {
	CustomerEmail string        `json:"customer_email"`
	Items         []events.Item `json:"items"`
}

// Validate reports why a request cannot become an order, or nil.
func Validate(r CreateOrderRequest) error {
	if !strings.Contains(r.CustomerEmail, "@") {
		return errors.New("customer_email must contain @")
	}
	if strings.ContainsRune(r.CustomerEmail, 0) {
		return errors.New("customer_email must not contain NUL")
	}
	if len(r.Items) < 1 || len(r.Items) > maxItems {
		return fmt.Errorf("items must have between 1 and %d entries", maxItems)
	}
	seen := make(map[string]bool, len(r.Items))
	for _, it := range r.Items {
		if it.SKU == "" {
			return errors.New("item sku must not be empty")
		}
		if strings.ContainsRune(it.SKU, 0) {
			return errors.New("item sku must not contain NUL")
		}
		if it.Qty < 1 || it.Qty > maxQty {
			return fmt.Errorf("item qty must be between 1 and %d", maxQty)
		}
		if seen[it.SKU] {
			return fmt.Errorf("duplicate sku %s", it.SKU)
		}
		seen[it.SKU] = true
	}
	return nil
}

// newRootMux serves the health endpoints and the API on one listener.
func newRootMux(health, api http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/healthz", health)
	mux.Handle("/readyz", health)
	mux.Handle("/", traced(api))
	return mux
}

// traced records a server span per API request, named after the route the
// inner mux matched. otelhttp renames the span once the handler returns, when
// r.Pattern is known; before that (and on a 404) the root pattern "/" gives
// way to the method alone.
func traced(api http.Handler) http.Handler {
	return otelhttp.NewHandler(api, "orders",
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			if r.Pattern == "" || r.Pattern == "/" {
				return r.Method
			}
			return r.Pattern
		}))
}

// Routes returns the API routes.
func (s *Service) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /orders", s.create)
	mux.HandleFunc("GET /orders/{id}", s.get)
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func (s *Service) create(w http.ResponseWriter, r *http.Request) {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	var req CreateOrderRequest
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := Validate(req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	id := uuid.New()
	trace.SpanFromContext(r.Context()).SetAttributes(attribute.String("order.id", id.String()))
	items, err := json.Marshal(req.Items)
	if err != nil {
		s.fail(r.Context(), w, "marshal items", err)
		return
	}
	if _, err := s.pool.Exec(r.Context(),
		`INSERT INTO orders (id, customer_email, items, status) VALUES ($1, $2, $3, $4)`,
		id, req.CustomerEmail, items, StatusPending); err != nil {
		s.fail(r.Context(), w, "insert order", err)
		return
	}

	out, err := events.New(events.OrderCreated, id,
		events.OrderCreatedPayload{CustomerEmail: req.CustomerEmail, Items: req.Items})
	if err == nil {
		// Detached from the client: a disconnect mid-publish must not leave us unsure
		// whether the broker accepted the event before we delete the order.
		pubCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), publishTimeout)
		defer cancel()
		err = events.Publish(pubCtx, s.js, out)
	}
	if err != nil {
		// Without the event the saga never starts: do not leave a PENDING order behind.
		if _, derr := s.pool.Exec(context.WithoutCancel(r.Context()), `DELETE FROM orders WHERE id = $1`, id); derr != nil {
			s.log.ErrorContext(r.Context(), "delete order after failed publish", "error", derr, "order_id", id)
		}
		s.fail(r.Context(), w, "publish orders.created", err)
		return
	}
	createdCounter.Add(r.Context(), 1)
	s.log.InfoContext(r.Context(), "order created", "order_id", id.String(), "items", len(req.Items))
	writeJSON(w, http.StatusAccepted, map[string]any{"id": id, "status": StatusPending})
}

func (s *Service) fail(ctx context.Context, w http.ResponseWriter, msg string, err error) {
	s.log.ErrorContext(ctx, msg, "error", err)
	writeError(w, http.StatusServiceUnavailable, "temporarily unavailable")
}

func (s *Service) get(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "id must be a UUID")
		return
	}
	trace.SpanFromContext(r.Context()).SetAttributes(attribute.String("order.id", id.String()))
	var (
		o      Order
		reason *string
	)
	err = s.pool.QueryRow(r.Context(),
		`SELECT id, status, reason, customer_email, items, amount_cents, created_at, updated_at
		 FROM orders WHERE id = $1`, id).
		Scan(&o.ID, &o.Status, &reason, &o.CustomerEmail, &o.Items, &o.AmountCents, &o.CreatedAt, &o.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		s.fail(r.Context(), w, "read order", err)
		return
	}
	if reason != nil {
		o.Reason = *reason
	}
	writeJSON(w, http.StatusOK, o)
}
