//go:build integration

// Package storetest starts a throwaway PostgreSQL for integration tests.
package storetest

import (
	"context"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/JoshuaMart/recon/internal/store"
)

// Image is the image the compose stack runs.
const Image = "postgres:18-alpine"

// Password is the throwaway container's password.
const Password = "recon-test-password"

// Start runs a container and returns it with its connection string.
func Start(t *testing.T) (*postgres.PostgresContainer, string) {
	t.Helper()
	ctx := context.Background()
	c, err := postgres.Run(ctx, Image,
		postgres.WithDatabase("recon"),
		postgres.WithUsername("recon"),
		postgres.WithPassword(Password),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(c) })
	url, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	return c, url
}

// New starts a container, migrates it, and returns an open store.
func New(t *testing.T) *store.Store {
	t.Helper()
	_, url := Start(t)
	st, err := store.Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if _, err := st.MigrateUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	return st
}
