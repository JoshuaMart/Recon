// Command controlplane serves the API and runs the background loops.
//
// It holds the application credential and never the owner's: role separation
// buys nothing if whoever reaches execution here finds both in the same
// environment. The configuration refuses to start if it is given both, which
// is where that property is enforced rather than in a comment.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JoshuaMart/recon/internal/api"
	"github.com/JoshuaMart/recon/internal/auth"
	"github.com/JoshuaMart/recon/internal/config"
	"github.com/JoshuaMart/recon/internal/enrich"
	"github.com/JoshuaMart/recon/internal/ingest"
	"github.com/JoshuaMart/recon/internal/maintenance"
	"github.com/JoshuaMart/recon/internal/obs"
	"github.com/JoshuaMart/recon/internal/runs"
	"github.com/JoshuaMart/recon/internal/store"
	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

func main() {
	// A distroless image has no shell and no curl, so the container's health
	// check is the binary asking itself. It reads the same configuration, which
	// means a probe cannot drift from what the process actually listens on.
	if len(os.Args) == 2 && os.Args[1] == "-health" {
		if err := probe(); err != nil {
			fmt.Fprintf(os.Stderr, "controlplane: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "controlplane: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(config.RoleControlPlane, config.Options{})
	if err != nil {
		return err
	}

	log := obs.NewLogger(os.Stderr, cfg.Log.Level, cfg.Log.Format)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := store.Open(ctx, cfg.Database.URL, cfg.Database.MaxConns, cfg.Database.ConnectTimeout)
	if err != nil {
		return err
	}
	defer pool.Close()

	signer, err := auth.NewSigner(cfg.Security.SigningKey)
	if err != nil {
		return err
	}

	enricher, err := enrich.Open(enrich.Paths{
		City: cfg.Enrich.CityDatabase,
		ASN:  cfg.Enrich.ASNDatabase,
	})
	if err != nil {
		return err
	}
	defer func() { _ = enricher.Close() }()
	log.InfoContext(ctx, "enrichment", "configured", enricher.Configured())

	scheduler := runs.New(signer, cfg.Verification, log)
	ingestor := ingest.New(enricher, scheduler.Cadence(), log)

	// Started unconditionally, and before anything can write: a partition job
	// behind a toggle is an ingestion outage three months later.
	go maintenance.New(sqlcgen.New(pool), cfg.Maintenance.Interval, log).Run(ctx)
	// The other loop that must not be optional. A run nothing expires holds
	// its targets forever, and the assets it froze are invisible to every
	// later tick.
	go runs.NewSweeper(scheduler, sqlcgen.New(pool), cfg.Verification.SweepInterval, log).Run(ctx)

	srv := &http.Server{
		Addr:              cfg.HTTP.Addr,
		Handler:           routes(pool, signer, scheduler, ingestor, log),
		ReadHeaderTimeout: cfg.HTTP.ReadTimeout,
	}

	errs := make(chan error, 1)
	go func() {
		log.InfoContext(ctx, "listening", "addr", cfg.HTTP.Addr, "env", cfg.Env)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
			return
		}
		errs <- nil
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		log.InfoContext(ctx, "shutting down", "grace", cfg.HTTP.ShutdownTimeout.String())
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.HTTP.ShutdownTimeout)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// probe asks the local listener whether it is alive.
func probe() error {
	cfg, err := config.Load(config.RoleControlPlane, config.Options{})
	if err != nil {
		return err
	}

	_, port, err := net.SplitHostPort(cfg.HTTP.Addr)
	if err != nil {
		return fmt.Errorf("read listen address: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://127.0.0.1:"+port+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check returned %s", resp.Status)
	}
	return nil
}

func routes(
	pool *pgxpool.Pool, signer *auth.Signer, scheduler *runs.Scheduler,
	ingestor *ingest.Ingestor, log *slog.Logger,
) http.Handler {
	mux := http.NewServeMux()

	// Where a run's report lands. It authenticates before reading anything
	// else, and the run comes from the credential rather than from the body.
	mux.Handle("POST /reports", api.NewReports(pool, signer, ingestor, log))

	// The only thing a run is given about the inventory, scoped to that run.
	mux.Handle("GET /runs/{run}/targets", api.NewTargets(pool, signer, log))

	// The console surface. Every route goes through one authorization layer
	// that produces a principal, even while there is one kind of caller.
	guard := api.NewGuard(pool, log)
	programs := api.NewPrograms(pool, scheduler, ingestor, log)
	mux.Handle("POST /programs/{program}/runs", guard.Require(auth.ActionManageJobs, programs.StartRun))
	// Entering an asset by hand is an assertion about the perimeter, which is
	// a different privilege from writing what a scanner found.
	mux.Handle("POST /programs/{program}/assets", guard.Require(auth.ActionManageScope, programs.EnterAssets))

	// Liveness. It touches nothing, because a probe that queries the database
	// turns a slow database into a restarted process.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// Readiness, which is the one that asks. Separate from the above so that
	// the two questions have two answers.
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		if err := pool.Ping(ctx); err != nil {
			log.ErrorContext(ctx, "readiness check failed", "error", err)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"status": "unavailable",
				"reason": "database",
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})

	return mux
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
