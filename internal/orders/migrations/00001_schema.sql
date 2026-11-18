-- +goose Up
CREATE TABLE orders (
    id             uuid PRIMARY KEY,
    customer_email text        NOT NULL,
    items          jsonb       NOT NULL,
    status         text        NOT NULL,
    reason         text,
    amount_cents   bigint,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE orders;
