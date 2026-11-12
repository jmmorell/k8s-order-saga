// Package httpx provides health endpoints and graceful HTTP serving.
package httpx

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"time"
)

const checkTimeout = 2 * time.Second

// ReadyCheck reports whether a dependency is ready; a non-nil error means not ready.
type ReadyCheck func(ctx context.Context) error

// HealthMux serves GET /healthz (liveness, always 200) and GET /readyz
// (200 when every check passes, otherwise 503 listing the failed check names, sorted).
func HealthMux(checks map[string]ReadyCheck) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		failed := runChecks(r.Context(), checks)
		if len(failed) > 0 {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "unavailable", "failed": failed})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})
	return mux
}

// runChecks runs all checks concurrently, each with its own timeout, and returns the sorted failed names.
func runChecks(ctx context.Context, checks map[string]ReadyCheck) []string {
	var (
		mu     sync.Mutex
		wg     sync.WaitGroup
		failed []string
	)
	for name, check := range checks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, checkTimeout)
			defer cancel()
			if err := check(cctx); err != nil {
				mu.Lock()
				failed = append(failed, name)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	sort.Strings(failed)
	return failed
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
