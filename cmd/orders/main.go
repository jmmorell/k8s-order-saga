// Command orders runs the orders service.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/jmmorell/k8s-order-saga/internal/orders"
	"github.com/jmmorell/k8s-order-saga/internal/platform/config"
	"github.com/jmmorell/k8s-order-saga/internal/platform/logx"
)

func main() {
	log := logx.New("orders", config.String("LOG_LEVEL", "info"))
	if err := run(log); err != nil {
		log.Error("orders stopped", "error", err)
		os.Exit(1)
	}
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
