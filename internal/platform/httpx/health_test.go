package httpx_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"k8s-portfolio/internal/platform/httpx"
)

func TestReadyz_FailingChecksReturn503WithSortedNames(t *testing.T) {
	mux := httpx.HealthMux(map[string]httpx.ReadyCheck{
		"nats": func(context.Context) error { return errors.New("down") },
		"db":   func(context.Context) error { return errors.New("down") },
		"ok":   func(context.Context) error { return nil },
	})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
	require.Equal(t, 503, rec.Code)
	require.JSONEq(t, `{"status":"unavailable","failed":["db","nats"]}`, rec.Body.String())
}

func TestReadyz_AllOK(t *testing.T) {
	mux := httpx.HealthMux(map[string]httpx.ReadyCheck{
		"db": func(context.Context) error { return nil },
	})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
	require.Equal(t, 200, rec.Code)
	require.JSONEq(t, `{"status":"ok"}`, rec.Body.String())
}

func TestReadyz_NoChecks(t *testing.T) {
	rec := httptest.NewRecorder()
	httpx.HealthMux(nil).ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
	require.Equal(t, 200, rec.Code)
}

func TestHealthz_AlwaysOK(t *testing.T) {
	mux := httpx.HealthMux(map[string]httpx.ReadyCheck{
		"db": func(context.Context) error { return errors.New("down") },
	})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	require.Equal(t, 200, rec.Code)
	require.JSONEq(t, `{"status":"ok"}`, rec.Body.String())
}
