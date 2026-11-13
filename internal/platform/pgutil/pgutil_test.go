package pgutil_test

import (
	"context"
	"embed"
	"errors"
	"io/fs"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"k8s-portfolio/internal/platform/pgutil"
	"k8s-portfolio/internal/platform/testinfra"
)

//go:embed testdata/*.sql
var testMigrations embed.FS

func TestConnect_GivesUpWhenContextEnds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	_, err := pgutil.Connect(ctx, "postgres://u:p@127.0.0.1:1/x")
	require.Error(t, err)
	require.Less(t, time.Since(start), 4*time.Second)
}

func TestMigrate_TwiceIsNoop(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool, err := pgutil.Connect(ctx, testinfra.Postgres(t, "app")["app"])
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	sub, err := fsSub()
	require.NoError(t, err)
	require.NoError(t, pgutil.Migrate(ctx, pool, sub))
	require.NoError(t, pgutil.Migrate(ctx, pool, sub))

	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM things`).Scan(&n))
	require.Equal(t, 1, n)
}

func TestMigrate_ConcurrentCallersAllSucceed(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	url := testinfra.Postgres(t, "app")["app"]
	sub, err := fsSub()
	require.NoError(t, err)

	// Each caller has its own pool, like separate replicas of a service.
	const callers = 8
	errs := make(chan error, callers)
	for range callers {
		pool, err := pgutil.Connect(ctx, url)
		require.NoError(t, err)
		t.Cleanup(pool.Close)
		go func() { errs <- pgutil.Migrate(ctx, pool, sub) }()
	}
	for range callers {
		require.NoError(t, <-errs)
	}
}

func TestInTx_CommitsOnNilRollsBackOnError(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool, err := pgutil.Connect(ctx, testinfra.Postgres(t, "app")["app"])
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `CREATE TABLE t (n int)`)
	require.NoError(t, err)

	require.NoError(t, pgutil.InTx(ctx, pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO t VALUES (1)`)
		return err
	}))
	boom := errors.New("boom")
	err = pgutil.InTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO t VALUES (2)`); err != nil {
			return err
		}
		return boom
	})
	require.ErrorIs(t, err, boom)

	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM t`).Scan(&n))
	require.Equal(t, 1, n)
}

func fsSub() (fs.FS, error) { return fs.Sub(testMigrations, "testdata") }
