// Command inventory runs the inventory service.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jmmorell/k8s-order-saga/internal/inventory"
	"github.com/jmmorell/k8s-order-saga/internal/platform/config"
	"github.com/jmmorell/k8s-order-saga/internal/platform/logx"
	"github.com/jmmorell/k8s-order-saga/internal/platform/telemetry"
)

func main() {
	os.Exit(realMain())
}

func realMain() int {
	// Telemetry first, so the logger below can pick up the global log provider.
	shutdown, err := telemetry.Setup(context.Background(), "inventory")
	if err != nil {
		fmt.Fprintln(os.Stderr, "inventory: telemetry setup:", err)
		return 1
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdown(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "inventory: telemetry shutdown:", err)
		}
	}()

	log := logx.New("inventory", config.String("LOG_LEVEL", "info"))
	if err := run(log); err != nil {
		log.Error("inventory stopped", "error", err)
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
	restock, err := config.Duration("RESTOCK_INTERVAL", time.Minute)
	if err != nil {
		return err
	}
	cfg := inventory.Config{
		HTTPAddr:        config.String("HTTP_ADDR", ":8080"),
		DatabaseURL:     dbURL,
		NATSURL:         natsURL,
		RestockInterval: restock,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := inventory.Run(ctx, cfg, log); err != nil {
		return fmt.Errorf("run: %w", err)
	}
	return nil
}
