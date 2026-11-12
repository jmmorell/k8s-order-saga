-- +goose Up
CREATE TABLE things (id int PRIMARY KEY);
INSERT INTO things VALUES (1);

-- +goose Down
DROP TABLE things;
