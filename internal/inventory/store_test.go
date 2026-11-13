package inventory_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/jmmorell/k8s-order-saga/internal/platform/events"
)

func TestSeed_ProductsAndPrices(t *testing.T) {
	e := setup(t)
	ctx := context.Background()

	var n int
	require.NoError(t, e.pool.QueryRow(ctx, `SELECT count(*) FROM products`).Scan(&n))
	require.Equal(t, 10, n)

	var price int64
	require.NoError(t, e.pool.QueryRow(ctx, `SELECT price_cents FROM products WHERE sku='SKU-009'`).Scan(&price))
	require.EqualValues(t, 4500, price)
	require.Equal(t, 1000, e.stock(t, "SKU-001"))
	require.Equal(t, 0, e.stock(t, "SKU-010"))
}

func TestSchema_RejectsNegativeStock(t *testing.T) {
	e := setup(t)
	_, err := e.pool.Exec(context.Background(), `UPDATE products SET stock = -1 WHERE sku='SKU-001'`)
	require.Error(t, err)
}

func TestOrderCreated_DuplicateSKUsAreSummed(t *testing.T) {
	e := setup(t)
	require.NoError(t, e.svc.HandleOrderCreated(context.Background(),
		orderCreated(t, uuid.New(), events.Item{SKU: "SKU-001", Qty: 2}, events.Item{SKU: "SKU-001", Qty: 3})))
	require.Equal(t, 995, e.stock(t, "SKU-001"))
}
