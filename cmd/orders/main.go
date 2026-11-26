// Command orders runs the orders service.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jmmorell/k8s-order-saga/internal/orders"
	"github.com/jmmorell/k8s-order-saga/internal/platform/config"
	"github.com/jmmorell/k8s-order-saga/internal/platform/logx"
	"github.com/jmmorell/k8s-order-saga/internal/platform/telemetry"
)

func main() {
	os.Exit(realMain())
}

func realMain() int {
	// Telemetry first, so the logger below can pick up the global log provider.
	shutdown, err := telemetry.Setup(context.Background(), "orders")
	if err != nil {
		fmt.Fprintln(os.Stderr, "orders: telemetry setup:", err)
		return 1
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdown(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "orders: telemetry shutdown:", err)
		}
	}()

	log := logx.New("orders", config.String("LOG_LEVEL", "info"))
	if err := run(log); err != nil {
		log.Error("orders stopped", "error", err)
		return 1
	}
	return 0
}

func run(log *slog.Logger) error {
	dbURL, err := config.Required("DATABASE_URL")
	if err != nil {
		return err
	}
	natsURL, err := config.Required("NATS_URL")
	if err != nil {
		return err
	}
	cfg := orders.Config{
		HTTPAddr:    config.String("HTTP_ADDR", ":8080"),
		DatabaseURL: dbURL,
		NATSURL:     natsURL,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := orders.Run(ctx, cfg, log); err != nil {
		return fmt.Errorf("run: %w", err)
	}
	return nil
}
