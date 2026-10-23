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
	"sync"
	"syscall"
	"time"

	"github.com/JoshuaMart/recon/internal/api"
	"github.com/JoshuaMart/recon/internal/config"
	"github.com/JoshuaMart/recon/internal/fingerprint"
	"github.com/JoshuaMart/recon/internal/ingest"
	"github.com/JoshuaMart/recon/internal/inventory"
	"github.com/JoshuaMart/recon/internal/lifecycle"
	"github.com/JoshuaMart/recon/internal/render"
	"github.com/JoshuaMart/recon/internal/runs"
	"github.com/JoshuaMart/recon/internal/scope"
	"github.com/JoshuaMart/recon/internal/store"
	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
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

// maintain keeps the monthly partitions two months ahead, daily.
func maintain(ctx context.Context, log *slog.Logger, st *store.Store) {
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := st.EnsurePartitions(ctx, time.Now()); err != nil {
				log.ErrorContext(ctx, "partition maintenance", "err", err)
			}
		}
	}
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

	vc := cfg.Verification
	cad := lifecycle.DefaultCadences
	cad.Resolve, cad.Full, cad.Dormant = vc.Resolve, vc.Full, vc.Dormant
	sigs := make([]ingest.Signature, 0, len(vc.DeadOrigins))
	for _, d := range vc.DeadOrigins {
		sigs = append(sigs, ingest.Signature{Name: d.Name, Statuses: d.Statuses, Server: d.Server, Title: d.Title})
	}
	sched := &runs.Scheduler{
		Store: st, Log: log, Slots: cfg.FastRecon.Slots, Timeout: cfg.FastRecon.Timeout,
		Ports: cfg.Discovery.Ports, Tick: 5 * time.Second, Now: time.Now,
		Verification: vc.Enabled, Batch: vc.Batch, ArchiveAfter: vc.ArchiveAfter, Cadences: &cad, Signatures: sigs,
	}
	if !vc.Enabled {
		log.InfoContext(ctx, "verification disabled: known hosts are not probed")
	}
	if f := cfg.FastRecon; f.URL != "" {
		sched.Runner = runs.NewServe(f.URL, f.APIToken, f.Timeout)
	} else {
		log.InfoContext(ctx, "no fastrecon.url: runs stay queued")
	}
	if err := sched.Recover(ctx); err != nil {
		return err
	}

	// The loops share one context and are waited for before the pool closes:
	// a loop still writing when it closes would lose what it was doing.
	loopsCtx, stopLoops := context.WithCancel(ctx)
	var loops sync.WaitGroup
	defer func() {
		stopLoops()
		loops.Wait()
	}()
	spawn := func(f func(context.Context)) {
		loops.Add(1)
		go func() {
			defer loops.Done()
			f(loopsCtx)
		}()
	}
	spawn(sched.Loop)
	if rc := cfg.Render; rc.URL != "" {
		storage := fingerprint.Dir{Root: cfg.Screenshots.Dir}
		disp := &render.Dispatcher{
			Writer: &render.Writer{
				Store: st, Storage: storage, Now: time.Now,
				Settings: render.Settings{
					Periodic: rc.Periodic, Retry: rc.Retry, Quality: cfg.Screenshots.Quality,
					Keep: cfg.Screenshots.Keep, Jitter: render.DefaultSettings.Jitter,
				},
			},
			Scanner: fingerprint.NewClient(rc.URL, rc.Timeout), Log: log, Max: rc.Concurrency, Tick: 2 * time.Second,
		}
		deps.Storage, deps.Render = storage, disp.Status
		spawn(disp.Loop)
	} else {
		log.InfoContext(ctx, "no render.url: renders stay due")
	}
	spawn(func(ctx context.Context) { maintain(ctx, log, st) })
	// Off the startup path: the API answers while the inventory catches up.
	spawn(func(ctx context.Context) {
		r, err := inventory.RefreshAll(ctx, st.Pool, st.Q, time.Now().UTC().Truncate(time.Microsecond))
		switch {
		case err != nil && ctx.Err() == nil:
			log.ErrorContext(ctx, "inventory refresh", "err", err)
		case r.Declared+r.Reclassified > 0:
			log.InfoContext(ctx, "inventory caught up with the rules", "declared", r.Declared, "reclassified", r.Reclassified)
		}
	})

	if se := cfg.ScopesExtractor; se.URL != "" {
		syncer := &scope.Syncer{
			Store: st, Source: scope.NewClient(se.URL, se.APIKey, se.Timeout), Log: log,
			MaxRemovedRatio: se.MaxRemovedRatio, Now: time.Now,
			Refresh: func(ctx context.Context, q *sqlcgen.Queries, p sqlcgen.Program, now time.Time) error {
				_, err := inventory.Refresh(ctx, q, p, cfg.Discovery.AutoScan, now)
				return err
			},
			Reschedule: inventory.SyncSchedule,
		}
		deps.Sync = syncer
		spawn(func(ctx context.Context) { syncer.Loop(ctx, se.Interval) })
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
