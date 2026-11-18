package events

import "github.com/google/uuid"

// Reasons carried by rejection, failure and cancellation events.
const (
	ReasonOutOfStock    = "out_of_stock"
	ReasonUnknownSKU    = "unknown_sku"
	ReasonPaymentFailed = "payment_failed"
	ReasonDeclined      = "declined"
)

type Item struct {
	SKU string `json:"sku"`
	Qty int    `json:"qty"`
}

type OrderCreatedPayload struct {
	CustomerEmail string `json:"customer_email"`
	Items         []Item `json:"items"`
}

type InventoryReservedPayload struct {
	Items       []Item `json:"items"`
	AmountCents int64  `json:"amount_cents"`
}

type InventoryRejectedPayload struct {
	Reason string `json:"reason"`
	SKU    string `json:"sku"`
}

type InventoryReleasedPayload struct{}

type PaymentSucceededPayload struct {
	PaymentID   uuid.UUID `json:"payment_id"`
	AmountCents int64     `json:"amount_cents"`
}

type PaymentFailedPayload struct {
	Reason string `json:"reason"`
}

type OrderConfirmedPayload struct {
	CustomerEmail string `json:"customer_email"`
	AmountCents   int64  `json:"amount_cents"`
}

type OrderCancelledPayload struct {
	CustomerEmail string `json:"customer_email"`
	Reason        string `json:"reason"`
}
