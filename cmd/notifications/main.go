// Command notifications runs the notifications service.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jmmorell/k8s-order-saga/internal/notifications"
	"github.com/jmmorell/k8s-order-saga/internal/platform/config"
	"github.com/jmmorell/k8s-order-saga/internal/platform/logx"
	"github.com/jmmorell/k8s-order-saga/internal/platform/telemetry"
)

func main() {
	os.Exit(realMain())
}

func realMain() int {
	// Telemetry first, so the logger below can pick up the global log provider.
	shutdown, err := telemetry.Setup(context.Background(), "notifications")
	if err != nil {
		fmt.Fprintln(os.Stderr, "notifications: telemetry setup:", err)
		return 1
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdown(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "notifications: telemetry shutdown:", err)
		}
	}()

	log := logx.New("notifications", config.String("LOG_LEVEL", "info"))
	if err := run(log); err != nil {
		log.Error("notifications stopped", "error", err)
		return 1
	}
	return 0
}

func run(log *slog.Logger) error {
	natsURL, err := config.Required("NATS_URL")
	if err != nil {
		return err
	}
	smtpAddr, err := config.Required("SMTP_ADDR")
	if err != nil {
		return err
	}
	cfg := notifications.Config{
		HTTPAddr: config.String("HTTP_ADDR", ":8080"),
		NATSURL:  natsURL,
		SMTPAddr: smtpAddr,
		From:     config.String("MAIL_FROM", "shop@example.local"),
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := notifications.Run(ctx, cfg, log); err != nil {
		return fmt.Errorf("run: %w", err)
	}
	return nil
}
