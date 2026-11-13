// Command payments runs the payments service.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jmmorell/k8s-order-saga/internal/payments"
	"github.com/jmmorell/k8s-order-saga/internal/platform/config"
	"github.com/jmmorell/k8s-order-saga/internal/platform/logx"
)

func main() {
	log := logx.New("payments", config.String("LOG_LEVEL", "info"))
	if err := run(log); err != nil {
		log.Error("payments stopped", "error", err)
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
	rate, err := config.Float("PAYMENT_FAILURE_RATE", 0.1)
	if err != nil {
		return err
	}
	latencyMS, err := config.Int("PAYMENT_LATENCY_MS", 200)
	if err != nil {
		return err
	}
	cfg := payments.Config{
		HTTPAddr:    config.String("HTTP_ADDR", ":8080"),
		DatabaseURL: dbURL,
		NATSURL:     natsURL,
		FailureRate: rate,
		Latency:     time.Duration(latencyMS) * time.Millisecond,
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := payments.Run(ctx, cfg, log); err != nil {
		return fmt.Errorf("run: %w", err)
	}
	return nil
}
