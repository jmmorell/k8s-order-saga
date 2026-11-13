// Command loadgen posts a steady stream of orders to the orders API.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"k8s-portfolio/internal/loadgen"
	"k8s-portfolio/internal/platform/config"
	"k8s-portfolio/internal/platform/logx"
)

func main() {
	log := logx.New("loadgen", config.String("LOG_LEVEL", "info"))
	if err := run(log); err != nil {
		log.Error("loadgen stopped", "error", err)
		os.Exit(1)
	}
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
