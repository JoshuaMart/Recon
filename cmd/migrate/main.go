// Command migrate applies and rolls back the schema, as the owner role. It is its
// own binary so a failed migration is readable alone and the deployment gates the
// application on it rather than on a remembered ordering. It and the operational CLI
// are the only processes holding the owner credential.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/jackc/pgx/v5"

	"github.com/JoshuaMart/recon/internal/config"
	"github.com/JoshuaMart/recon/internal/obs"
	"github.com/JoshuaMart/recon/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
		os.Exit(1)
	}
}

func usage() error {
	return errors.New("usage: migrate up|down|reset|status|version|seed")
}

func run() error {
	if len(os.Args) != 2 {
		return usage()
	}
	command := os.Args[1]

	cfg, err := config.Load(config.RoleMigrate, config.Options{})
	if err != nil {
		return err
	}

	log := obs.NewLogger(os.Stderr, cfg.Log.Level, cfg.Log.Format)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	migrator, err := store.NewMigrator(cfg.Database.MigrationURL, log)
	if err != nil {
		return err
	}
	defer func() { _ = migrator.Close() }()

	switch command {
	case "up":
		return migrator.Run(ctx, store.Up)
	case "down":
		return migrator.Run(ctx, store.Down)
	case "reset":
		return migrator.Run(ctx, store.Reset)
	case "version":
		version, err := migrator.Version(ctx)
		if err != nil {
			return err
		}
		fmt.Println(version)
		return nil
	case "seed":
		// Reference data, reapplied on every deployment rather than by a one-off migration,
		// as the lists grow with new frameworks and edges. It runs here because it runs as
		// the owner; the application role has no write on those tables.
		conn, err := pgx.Connect(ctx, cfg.Database.MigrationURL)
		if err != nil {
			return fmt.Errorf("connect: %w", err)
		}
		defer func() { _ = conn.Close(context.Background()) }()

		written, err := store.Seed(ctx, conn)
		if err != nil {
			return err
		}
		log.InfoContext(ctx, "reference data applied", "entries", written)
		return nil
	case "status":
		status, err := migrator.Status(ctx)
		if err != nil {
			return err
		}
		for _, s := range status {
			state := "pending"
			if s.State == "applied" {
				state = "applied"
			}
			fmt.Printf("%-8s %d %s\n", state, s.Source.Version, s.Source.Path)
		}
		return nil
	default:
		return usage()
	}
}
