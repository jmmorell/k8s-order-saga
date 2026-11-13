package loadgen_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"k8s-portfolio/internal/loadgen"
	"k8s-portfolio/internal/orders"
)

func TestRandomOrder_AlwaysValid(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for n := range 1000 {
		o := loadgen.RandomOrder(r, 0.3, n)
		require.NoError(t, orders.Validate(o), "order %d: %+v", n, o)
	}
}

func TestRandomOrder_RatioOneIsAlwaysSKU010(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for n := range 100 {
		o := loadgen.RandomOrder(r, 1, n)
		require.Len(t, o.Items, 1)
		require.Equal(t, "SKU-010", o.Items[0].SKU)
	}
}

func TestRandomOrder_RatioZeroNeverSKU010(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for n := range 1000 {
		for _, it := range loadgen.RandomOrder(r, 0, n).Items {
			require.NotEqual(t, "SKU-010", it.SKU)
		}
	}
}

func TestRandomOrder_EmailUsesCounter(t *testing.T) {
	o := loadgen.RandomOrder(rand.New(rand.NewPCG(1, 2)), 0, 7)
	require.Equal(t, "loadgen+7@example.local", o.CustomerEmail)
}

func TestRun_PostsOrdersAtRate(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/orders" {
			var req orders.CreateOrderRequest
			if json.NewDecoder(r.Body).Decode(&req) == nil && orders.Validate(req) == nil {
				hits.Add(1)
			}
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- loadgen.Run(ctx, loadgen.Config{HTTPAddr: "127.0.0.1:0", TargetURL: srv.URL, Rate: 50},
			slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()

	require.Eventually(t, func() bool { return hits.Load() >= 3 }, 5*time.Second, 20*time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}
