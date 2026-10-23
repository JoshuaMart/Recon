// Command recon is the whole platform in one binary.
//
//	recon serve   [-config file]        migrate, then serve the API
//	recon migrate [-config file] up|down|status
//	recon version
//	recon healthcheck [-url url]       the container probe
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/JoshuaMart/recon/internal/api"
	"github.com/JoshuaMart/recon/internal/config"
	"github.com/JoshuaMart/recon/internal/scope"
	"github.com/JoshuaMart/recon/internal/store"
	"github.com/JoshuaMart/recon/internal/version"
)

const usage = `usage:
  recon serve   [-config file]
  recon migrate [-config file] up|down|status
  recon version
  recon healthcheck [-url url]`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdout)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "recon:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	cmd, args := args[0], args[1:]
	switch cmd {
	case "version":
		_, err := fmt.Fprintln(out, version.Version)
		return err
	case "healthcheck":
		return healthcheck(ctx, args)
	case "serve", "migrate":
	default:
		return fmt.Errorf("unknown command %q\n%s", cmd, usage)
	}

	fset := flag.NewFlagSet(cmd, flag.ContinueOnError)
	configPath := fset.String("config", "", "YAML config file")
	if err := fset.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	log := newLogger(cfg.Log, out)

	st, err := store.Open(ctx, cfg.Database.URL)
	if err != nil {
		return err
	}
	defer st.Close()

	if cmd == "migrate" {
		return migrate(ctx, log, st, fset.Args())
	}
	return serve(ctx, log, cfg, st)
}

// healthcheck is the container probe: the runtime image has no shell or curl.
func healthcheck(ctx context.Context, args []string) error {
	fset := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	url := fset.String("url", "http://localhost:8080/api/health", "health endpoint")
	if err := fset.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, *url, nil)
	if err != nil {
		return err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("health: %s", res.Status)
	}
	return nil
}

func newLogger(c config.Log, out io.Writer) *slog.Logger {
	var level slog.Level
	_ = level.UnmarshalText([]byte(c.Level))
	opts := &slog.HandlerOptions{Level: level}
	if c.Format == "text" {
		return slog.New(slog.NewTextHandler(out, opts))
	}
	return slog.New(slog.NewJSONHandler(out, opts))
}

func migrate(ctx context.Context, log *slog.Logger, st *store.Store, args []string) error {
	if len(args) != 1 {
		return errors.New(usage)
	}
	p, sqlDB, err := st.Migrator()
	if err != nil {
		return err
	}
	defer sqlDB.Close()

	switch args[0] {
	case "up":
		_, err = p.Up(ctx)
	case "down":
		_, err = p.Down(ctx)
	case "status":
		statuses, serr := p.Status(ctx)
		if serr != nil {
			return serr
		}
		for _, s := range statuses {
			log.InfoContext(ctx, "migration", "version", s.Source.Version, "path", s.Source.Path, "state", s.State)
		}
		return nil
	default:
		return fmt.Errorf("unknown migrate action %q\n%s", args[0], usage)
	}
	if err != nil {
		return err
	}
	v, err := p.GetDBVersion(ctx)
	if err != nil {
		return err
	}
	log.InfoContext(ctx, "migrated", "action", args[0], "version", v)
	return nil
}

func serve(ctx context.Context, log *slog.Logger, cfg config.Config, st *store.Store) error {
	v, err := st.MigrateUp(ctx)
	if err != nil {
		return err
	}
	log.InfoContext(ctx, "database ready", "schema_version", v)

	auth, err := api.NewAuth(cfg.APIKey, cfg.PublicURL)
	if err != nil {
		return err
	}
	deps := api.Deps{Log: log, DB: st, Store: st, Auth: auth, DefaultAutoScan: cfg.Discovery.AutoScan}

	if se := cfg.ScopesExtractor; se.URL != "" {
		syncer := &scope.Syncer{
			Store: st, Source: scope.NewClient(se.URL, se.APIKey, se.Timeout), Log: log,
			MaxRemovedRatio: se.MaxRemovedRatio, Now: time.Now,
		}
		deps.Sync = syncer
		go syncer.Loop(ctx, se.Interval)
	} else {
		log.InfoContext(ctx, "scope sync disabled: no scopes_extractor.url, manual programs only")
	}

	srv := &http.Server{
		Addr:              cfg.HTTP.Addr,
		Handler:           api.Handler(deps),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 1)
	go func() {
		log.InfoContext(ctx, "listening", "addr", cfg.HTTP.Addr, "version", version.Version)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
