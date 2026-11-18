package events

import (
	"context"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

var streams = []jetstream.StreamConfig{
	{Name: "ORDERS", Subjects: []string{"orders.*"}},
	{Name: "INVENTORY", Subjects: []string{"inventory.*"}},
	{Name: "PAYMENTS", Subjects: []string{"payments.*"}},
	{Name: "DLQ", Subjects: []string{"dlq.>"}},
}

// EnsureStreams creates or updates the four streams. It is idempotent, so
// every service can call it at startup.
func EnsureStreams(ctx context.Context, js jetstream.JetStream) error {
	for _, cfg := range streams {
		cfg.Storage = jetstream.FileStorage
		cfg.MaxAge = 7 * 24 * time.Hour
		cfg.Duplicates = 2 * time.Minute
		if _, err := js.CreateOrUpdateStream(ctx, cfg); err != nil {
			return fmt.Errorf("ensure stream %s: %w", cfg.Name, err)
		}
	}
	return nil
}
