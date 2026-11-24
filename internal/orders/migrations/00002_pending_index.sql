-- +goose Up
CREATE INDEX orders_pending_created_at ON orders (created_at) WHERE status = 'PENDING';

-- +goose Down
DROP INDEX orders_pending_created_at;
