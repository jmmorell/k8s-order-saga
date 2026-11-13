-- +goose Up
CREATE TABLE payments (
    id           uuid PRIMARY KEY,
    order_id     uuid        NOT NULL UNIQUE,
    amount_cents bigint      NOT NULL,
    status       text        NOT NULL CHECK (status IN ('SUCCEEDED', 'FAILED')),
    reason       text,
    created_at   timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE payments;
