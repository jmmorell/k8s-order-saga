// Package testinfra starts throwaway infrastructure containers for
// integration tests. Every helper registers its own cleanup.
package testinfra

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

const startTimeout = 90 * time.Second

func start(t *testing.T, req testcontainers.ContainerRequest) (testcontainers.Container, string) {
	t.Helper()
	ctx := context.Background()
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	testcontainers.CleanupContainer(t, c)
	if err != nil {
		t.Fatalf("start %s: %v", req.Image, err)
	}
	host, err := c.Host(ctx)
	if err != nil {
		t.Fatalf("container host: %v", err)
	}
	return c, host
}

func mapped(t *testing.T, c testcontainers.Container, host, port string) string {
	t.Helper()
	p, err := c.MappedPort(context.Background(), port)
	if err != nil {
		t.Fatalf("mapped port %s: %v", port, err)
	}
	return fmt.Sprintf("%s:%s", host, p.Port())
}

// NATS starts a JetStream-enabled NATS server and returns its URL.
func NATS(t *testing.T) (url string) {
	t.Helper()
	c, host := start(t, testcontainers.ContainerRequest{
		Image:        "nats:2.11-alpine",
		Cmd:          []string{"-js"},
		ExposedPorts: []string{"4222/tcp"},
		WaitingFor:   wait.ForLog("Server is ready").WithStartupTimeout(startTimeout),
	})
	return "nats://" + mapped(t, c, host, "4222/tcp")
}

// Postgres starts one PostgreSQL server, creates each named database and
// returns a map from database name to its connection URL.
func Postgres(t *testing.T, dbs ...string) map[string]string {
	t.Helper()
	const user, password = "test", "test"
	c, host := start(t, testcontainers.ContainerRequest{
		Image:        "postgres:17-alpine",
		Env:          map[string]string{"POSTGRES_USER": user, "POSTGRES_PASSWORD": password, "POSTGRES_DB": "postgres"},
		ExposedPorts: []string{"5432/tcp"},
		WaitingFor: wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).WithStartupTimeout(startTimeout),
	})
	addr := mapped(t, c, host, "5432/tcp")
	url := func(db string) string {
		return fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable", user, password, addr, db)
	}

	ctx := context.Background()
	admin, err := pgx.Connect(ctx, url("postgres"))
	if err != nil {
		t.Fatalf("connect to postgres: %v", err)
	}
	defer func() { _ = admin.Close(ctx) }()

	urls := make(map[string]string, len(dbs))
	for _, db := range dbs {
		if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{db}.Sanitize()); err != nil {
			t.Fatalf("create database %s: %v", db, err)
		}
		urls[db] = url(db)
	}
	return urls
}

// Mailpit starts a Mailpit SMTP sink and returns its SMTP address and HTTP API URL.
func Mailpit(t *testing.T) (smtpAddr, apiURL string) {
	t.Helper()
	c, host := start(t, testcontainers.ContainerRequest{
		Image:        "axllent/mailpit",
		ExposedPorts: []string{"1025/tcp", "8025/tcp"},
		WaitingFor:   wait.ForHTTP("/api/v1/info").WithPort("8025/tcp").WithStartupTimeout(startTimeout),
	})
	return mapped(t, c, host, "1025/tcp"), "http://" + mapped(t, c, host, "8025/tcp")
}
