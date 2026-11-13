// Package loadgen posts a steady stream of random orders to the orders API.
package loadgen

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"k8s-portfolio/internal/platform/httpx"
)

const (
	requestTimeout = 5 * time.Second
	reportInterval = 30 * time.Second
)

// Config holds the settings of the load generator.
type Config struct {
	HTTPAddr        string
	TargetURL       string
	Rate            float64 // orders per second
	OutOfStockRatio float64 // probability in [0,1] that an order asks for an out-of-stock SKU
}

// Validate reports whether the settings are usable.
func (c Config) Validate() error {
	if c.Rate <= 0 {
		return fmt.Errorf("loadgen: rate %v must be positive", c.Rate)
	}
	if c.OutOfStockRatio < 0 || c.OutOfStockRatio > 1 {
		return fmt.Errorf("loadgen: out-of-stock ratio %v is outside [0,1]", c.OutOfStockRatio)
	}
	return nil
}

// counts tallies responses by status code; 0 stands for a transport error.
type counts struct {
	mu sync.Mutex
	m  map[int]int
}

func (c *counts) add(code int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[code]++
}

// drain returns the tallies since the last call as "code=count" attributes.
func (c *counts) drain() []any {
	c.mu.Lock()
	snap := c.m
	c.m = map[int]int{}
	c.mu.Unlock()
	codes := make([]int, 0, len(snap))
	for code := range snap {
		codes = append(codes, code)
	}
	sort.Ints(codes)
	attrs := make([]any, 0, len(codes))
	for _, code := range codes {
		name := strconv.Itoa(code)
		if code == 0 {
			name = "error"
		}
		attrs = append(attrs, slog.Int(name, snap[code]))
	}
	return attrs
}

// Run posts orders until ctx ends. It returns nil on a clean stop.
func Run(ctx context.Context, cfg Config, log *slog.Logger) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	url := strings.TrimRight(cfg.TargetURL, "/") + "/orders"
	client := &http.Client{Timeout: requestTimeout}
	r := rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
	tally := &counts{m: map[int]int{}}

	// Posting is slow relative to the tick, so it runs apart from the loop
	// that keeps the rate steady; wg lets Run wait for requests in flight.
	var wg sync.WaitGroup
	srvErr := make(chan error, 1)
	srvCtx, stopSrv := context.WithCancel(ctx)
	defer stopSrv()
	go func() {
		srvErr <- httpx.Serve(srvCtx, cfg.HTTPAddr, httpx.HealthMux(nil), log)
	}()

	tick := time.NewTicker(time.Duration(float64(time.Second) / cfg.Rate))
	defer tick.Stop()
	report := time.NewTicker(reportInterval)
	defer report.Stop()

	for n := 1; ; n++ {
		select {
		case <-ctx.Done():
			wg.Wait()
			stopSrv()
			return <-srvErr
		case err := <-srvErr:
			wg.Wait()
			return err
		case <-report.C:
			log.Info("responses in the last 30s", tally.drain()...)
		case <-tick.C:
			body, err := json.Marshal(RandomOrder(r, cfg.OutOfStockRatio, n))
			if err != nil {
				return fmt.Errorf("marshal order: %w", err)
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				tally.add(post(ctx, client, url, body, log))
			}()
		}
	}
}

// post sends one order and returns the status code, or 0 on a transport error.
func post(ctx context.Context, c *http.Client, url string, body []byte, log *slog.Logger) int {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		log.Error("build request", "error", err)
		return 0
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			log.Warn("post order", "error", err)
		}
		return 0
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}
