// Command inventory runs the inventory service.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s-portfolio/internal/inventory"
	"k8s-portfolio/internal/platform/config"
	"k8s-portfolio/internal/platform/logx"
)

func main() {
	log := logx.New("inventory", config.String("LOG_LEVEL", "info"))
	if err := run(); err != nil {
		log.Error("inventory stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
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
	log := logx.New("inventory", config.String("LOG_LEVEL", "info"))
	if err := inventory.Run(ctx, cfg, log); err != nil {
		return fmt.Errorf("run: %w", err)
	}
	return nil
}
