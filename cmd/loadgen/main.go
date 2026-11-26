// Command loadgen posts a steady stream of orders to the orders API.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jmmorell/k8s-order-saga/internal/loadgen"
	"github.com/jmmorell/k8s-order-saga/internal/platform/config"
	"github.com/jmmorell/k8s-order-saga/internal/platform/logx"
	"github.com/jmmorell/k8s-order-saga/internal/platform/telemetry"
)

func main() {
	os.Exit(realMain())
}

func realMain() int {
	// Telemetry first, so the logger below can pick up the global log provider.
	shutdown, err := telemetry.Setup(context.Background(), "loadgen")
	if err != nil {
		fmt.Fprintln(os.Stderr, "loadgen: telemetry setup:", err)
		return 1
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdown(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "loadgen: telemetry shutdown:", err)
		}
	}()

	log := logx.New("loadgen", config.String("LOG_LEVEL", "info"))
	if err := run(log); err != nil {
		log.Error("loadgen stopped", "error", err)
		return 1
	}
	return 0
}

func run(log *slog.Logger) error {
	target, err := config.Required("TARGET_URL")
	if err != nil {
		return err
	}
	rate, err := config.Float("RATE_PER_SEC", 2)
	if err != nil {
		return err
	}
	ratio, err := config.Float("OUT_OF_STOCK_RATIO", 0.05)
	if err != nil {
		return err
	}
	cfg := loadgen.Config{
		HTTPAddr:        config.String("HTTP_ADDR", ":8080"),
		TargetURL:       target,
		Rate:            rate,
		OutOfStockRatio: ratio,
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := loadgen.Run(ctx, cfg, log); err != nil {
		return fmt.Errorf("run: %w", err)
	}
	return nil
}
