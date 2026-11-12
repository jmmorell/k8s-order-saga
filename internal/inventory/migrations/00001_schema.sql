-- +goose Up
CREATE TABLE products (
    sku         text PRIMARY KEY,
    name        text   NOT NULL,
    price_cents bigint NOT NULL,
    stock       int    NOT NULL CHECK (stock >= 0),
    restock_to  int    NOT NULL
);

CREATE TABLE reservations (
    order_id     uuid PRIMARY KEY,
    items        jsonb       NOT NULL,
    status       text        NOT NULL CHECK (status IN ('RESERVED', 'REJECTED', 'RELEASED')),
    reason       text,
    sku          text,
    amount_cents bigint,
    created_at   timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE reservations;
DROP TABLE products;
