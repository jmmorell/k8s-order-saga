package loadgen

import (
	"fmt"
	"math/rand/v2"

	"github.com/jmmorell/k8s-order-saga/internal/orders"
	"github.com/jmmorell/k8s-order-saga/internal/platform/events"
)

const (
	outOfStockSKU = "SKU-010"
	stockedSKUs   = 9 // SKU-001 .. SKU-009
	maxLines      = 3
	maxQty        = 3
)

// RandomOrder builds a valid order request. With probability outOfStockRatio
// it asks for one unit of SKU-010 (which the inventory never stocks);
// otherwise it picks 1 to 3 distinct SKUs from SKU-001..SKU-009, quantity 1 to 3.
func RandomOrder(r *rand.Rand, outOfStockRatio float64, n int) orders.CreateOrderRequest {
	req := orders.CreateOrderRequest{CustomerEmail: fmt.Sprintf("loadgen+%d@example.local", n)}
	if r.Float64() < outOfStockRatio {
		req.Items = []events.Item{{SKU: outOfStockSKU, Qty: 1}}
		return req
	}
	lines := 1 + r.IntN(maxLines)
	for _, i := range r.Perm(stockedSKUs)[:lines] {
		req.Items = append(req.Items, events.Item{SKU: fmt.Sprintf("SKU-%03d", i+1), Qty: 1 + r.IntN(maxQty)})
	}
	return req
}
