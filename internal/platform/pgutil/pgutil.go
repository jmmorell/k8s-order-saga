// Package pgutil holds the PostgreSQL helpers shared by services: connecting
// with retries, running embedded migrations and transactions.
package pgutil

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

const retryInterval = time.Second

// Connect opens a pool and pings it every second until it answers or ctx ends.
func Connect(ctx context.Context, url string) (*pgxpool.Pool, error) {
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	config.ConnConfig.Tracer = otelpgx.NewTracer()
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	for {
		err := pool.Ping(ctx)
		if err == nil {
			return pool, nil
		}
		select {
		case <-ctx.Done():
			pool.Close()
			return nil, fmt.Errorf("connect to postgres: %w", errors.Join(ctx.Err(), err))
		case <-time.After(retryInterval):
		}
	}
}

// Migrate applies the goose SQL migrations found at the root of migrations.
// Running it again when everything is applied does nothing.
func Migrate(ctx context.Context, pool *pgxpool.Pool, migrations fs.FS) error {
	db := stdlib.OpenDBFromPool(pool)
	defer func() { _ = db.Close() }()

	// An advisory lock makes several replicas migrating at once safe.
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return fmt.Errorf("create migration locker: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations,
		goose.WithSessionLocker(locker))
	if err != nil {
		return fmt.Errorf("create migration provider: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

// InTx runs fn in a transaction: commit when it returns nil, roll back otherwise.
func InTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}
