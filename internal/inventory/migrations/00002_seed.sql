-- +goose Up
INSERT INTO products (sku, name, price_cents, stock, restock_to)
SELECT 'SKU-' || lpad(n::text, 3, '0'), 'Product ' || n, n * 500, 1000, 1000
FROM generate_series(1, 9) AS n;

-- SKU-010 is always out of stock.
INSERT INTO products (sku, name, price_cents, stock, restock_to)
VALUES ('SKU-010', 'Product 10', 5000, 0, 0);

-- +goose Down
DELETE FROM products WHERE sku IN (SELECT 'SKU-' || lpad(n::text, 3, '0') FROM generate_series(1, 10) AS n);
