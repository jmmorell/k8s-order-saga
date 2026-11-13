package inventory

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jmmorell/k8s-order-saga/internal/platform/events"
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

// Reservation statuses.
const (
	statusReserved = "RESERVED"
	statusRejected = "REJECTED"
	statusReleased = "RELEASED"
)

// reservation is the stored outcome for one order.
type reservation struct {
	OrderID     uuid.UUID
	Items       []events.Item
	Status      string
	Reason      string
	SKU         string
	AmountCents int64
}

// getReservation loads and locks the reservation of an order; found is false when there is none.
func getReservation(ctx context.Context, tx pgx.Tx, orderID uuid.UUID) (r reservation, found bool, err error) {
	var items []byte
	var reason, sku *string
	var amount *int64
	err = tx.QueryRow(ctx,
		`SELECT items, status, reason, sku, amount_cents FROM reservations WHERE order_id = $1 FOR UPDATE`,
		orderID).Scan(&items, &r.Status, &reason, &sku, &amount)
	if errors.Is(err, pgx.ErrNoRows) {
		return reservation{}, false, nil
	}
	if err != nil {
		return reservation{}, false, fmt.Errorf("select reservation: %w", err)
	}
	if err := json.Unmarshal(items, &r.Items); err != nil {
		return reservation{}, false, fmt.Errorf("decode stored items: %w", err)
	}
	r.OrderID = orderID
	r.Reason, r.SKU = deref(reason), deref(sku)
	if amount != nil {
		r.AmountCents = *amount
	}
	return r, true, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func insertReservation(ctx context.Context, tx pgx.Tx, r reservation) error {
	items, err := json.Marshal(r.Items)
	if err != nil {
		return fmt.Errorf("encode items: %w", err)
	}
	var reason, sku *string
	var amount *int64
	if r.Reason != "" {
		reason = &r.Reason
	}
	if r.SKU != "" {
		sku = &r.SKU
	}
	if r.Status == statusReserved {
		amount = &r.AmountCents
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO reservations (order_id, items, status, reason, sku, amount_cents) VALUES ($1, $2, $3, $4, $5, $6)`,
		r.OrderID, items, r.Status, reason, sku, amount); err != nil {
		return fmt.Errorf("insert reservation: %w", err)
	}
	return nil
}

func setStatus(ctx context.Context, tx pgx.Tx, orderID uuid.UUID, status string) error {
	if _, err := tx.Exec(ctx, `UPDATE reservations SET status = $2 WHERE order_id = $1`, orderID, status); err != nil {
		return fmt.Errorf("update reservation status: %w", err)
	}
	return nil
}

type product struct {
	priceCents int64
	stock      int
}

// lockProducts locks the given SKUs in SKU order (so concurrent orders cannot
// deadlock) and returns those that exist.
func lockProducts(ctx context.Context, tx pgx.Tx, skus []string) (map[string]product, error) {
	rows, err := tx.Query(ctx,
		`SELECT sku, price_cents, stock FROM products WHERE sku = ANY($1) ORDER BY sku FOR UPDATE`, skus)
	if err != nil {
		return nil, fmt.Errorf("lock products: %w", err)
	}
	defer rows.Close()
	found := make(map[string]product, len(skus))
	for rows.Next() {
		var sku string
		var p product
		if err := rows.Scan(&sku, &p.priceCents, &p.stock); err != nil {
			return nil, fmt.Errorf("scan product: %w", err)
		}
		found[sku] = p
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read products: %w", err)
	}
	return found, nil
}

func adjustStock(ctx context.Context, tx pgx.Tx, sku string, delta int) error {
	if _, err := tx.Exec(ctx, `UPDATE products SET stock = stock + $2 WHERE sku = $1`, sku, delta); err != nil {
		return fmt.Errorf("adjust stock of %s: %w", sku, err)
	}
	return nil
}

// totals sums the quantity per SKU; the SKUs come back sorted.
func totals(items []events.Item) (skus []string, qty map[string]int) {
	qty = make(map[string]int, len(items))
	for _, it := range items {
		qty[it.SKU] += it.Qty
	}
	for sku := range qty {
		skus = append(skus, sku)
	}
	slices.Sort(skus)
	return skus, qty
}

// reserve decides the outcome of an order inside tx: it takes the stock and
// returns a RESERVED reservation, or returns a REJECTED one and touches no stock.
func reserve(ctx context.Context, tx pgx.Tx, orderID uuid.UUID, items []events.Item) (reservation, error) {
	skus, qty := totals(items)
	products, err := lockProducts(ctx, tx, skus)
	if err != nil {
		return reservation{}, err
	}
	rejected := func(reason, sku string) reservation {
		return reservation{OrderID: orderID, Items: items, Status: statusRejected, Reason: reason, SKU: sku}
	}
	for _, sku := range skus {
		if _, ok := products[sku]; !ok {
			return rejected(events.ReasonUnknownSKU, sku), nil
		}
	}
	for _, sku := range skus {
		if products[sku].stock < qty[sku] {
			return rejected(events.ReasonOutOfStock, sku), nil
		}
	}
	var amount int64
	for _, sku := range skus {
		if err := adjustStock(ctx, tx, sku, -qty[sku]); err != nil {
			return reservation{}, err
		}
		amount += products[sku].priceCents * int64(qty[sku])
	}
	return reservation{OrderID: orderID, Items: items, Status: statusReserved, AmountCents: amount}, nil
}

// release gives the reserved stock back to the products.
func release(ctx context.Context, tx pgx.Tx, items []events.Item) error {
	skus, qty := totals(items)
	if _, err := lockProducts(ctx, tx, skus); err != nil {
		return err
	}
	for _, sku := range skus {
		if err := adjustStock(ctx, tx, sku, qty[sku]); err != nil {
			return err
		}
	}
	return nil
}

func restock(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, `UPDATE products SET stock = restock_to`); err != nil {
		return fmt.Errorf("restock: %w", err)
	}
	return nil
}
