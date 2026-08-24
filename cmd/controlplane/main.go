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
	"github.com/JoshuaMart/recon/internal/ct"
	"github.com/JoshuaMart/recon/internal/enrich"
	"github.com/JoshuaMart/recon/internal/external"
	"github.com/JoshuaMart/recon/internal/fingerprint"
	"github.com/JoshuaMart/recon/internal/ingest"
	"github.com/JoshuaMart/recon/internal/maintenance"
	"github.com/JoshuaMart/recon/internal/notify"
	"github.com/JoshuaMart/recon/internal/obs"
	"github.com/JoshuaMart/recon/internal/render"
	"github.com/JoshuaMart/recon/internal/runner"
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

	// Two pools, and that is the whole of the isolation being structural: the
	// role a component connects with is chosen here, once, rather than case by
	// case in the code. Everything a request does runs on the first; the loops
	// that serve every tenant in one tick run on the second and say so by the
	// role they hold.
	appPool, err := store.Open(ctx, cfg.Database.URL, cfg.Database.MaxConns, cfg.Database.ConnectTimeout)
	if err != nil {
		return err
	}
	defer appPool.Close()
	scoped := store.NewScoped(appPool)

	// Its own bound, and a smaller one. Both pools reading one setting means a
	// deployment tuned for N backends quietly opens 2N.
	system, err := store.Open(ctx, cfg.Database.SystemURL, cfg.Database.SystemMaxConns,
		cfg.Database.ConnectTimeout)
	if err != nil {
		return err
	}
	defer system.Close()

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

	platform, err := startPlatform(cfg, log)
	if err != nil {
		return err
	}
	scheduler := runs.New(signer, cfg.Verification, log, runs.WithPlatform(platform))
	ingestor := ingest.New(enricher, scheduler.Cadence(), log)

	// Started unconditionally, and before anything can write: a partition job
	// behind a toggle is an ingestion outage three months later.
	go maintenance.New(sqlcgen.New(system), cfg.Maintenance.Interval, log).Run(ctx)
	// The other loop that must not be optional. A run nothing expires holds
	// its targets forever, and the assets it froze are invisible to every
	// later tick.
	go runs.NewSweeper(scheduler, sqlcgen.New(system), cfg.Verification.SweepInterval, log).Run(ctx)
	// The pass that provisions enumeration, distinct from the one on due dates.
	go runs.NewCadence(system, scheduler, cfg.Verification.SweepInterval, log).Run(ctx)
	// And the one on due dates, which is what turns a schedule into a run. The
	// two answer different questions: enumeration asks what exists under a
	// perimeter, this asks what still answers. Without it a due date is written
	// by every ingestion and nothing ever reads it, so the inventory is verified
	// exactly when somebody presses a button.
	go runs.NewDuePass(system, scheduler, cfg.Verification.SweepInterval, log).Run(ctx)
	// The candidate lane, which exists because of what it does not wait for. A
	// Certificate Transparency candidate is due a minute after it is created,
	// and one live verification run holds its slot for a whole deadline: on the
	// pass above, a candidate arriving mid sweep waits half an hour for a check
	// the aggressive curve wanted at sixty seconds.
	go runs.NewCandidatePass(system, scheduler, cfg.Verification.SweepInterval, log).Run(ctx)

	// The one loop that asks a question about somebody else's domain, and the
	// only outbound work in the control plane. It resolves an apex and nothing
	// else: no connection, no request, no render. An interval of zero turns it
	// off, for a deployment that would rather make no outbound query at all.
	if cfg.External.Interval > 0 {
		go external.New(system, external.NewDNS(cfg.External.Timeout),
			cfg.External.Interval, cfg.External.Batch, log).Run(ctx)
	} else {
		log.WarnContext(ctx, "the external host sweep is off, so an expired third party domain "+
			"will only be found where the host is in this inventory")
	}

	// The only asynchronous half of the alert path. Producing an event is
	// ingestion's job, in its transaction; this can lag without consequence.
	//
	// The configured channel becomes the single row that carries it, and only
	// where exactly one organization exists: a configuration file cannot name a
	// tenant. The loop retries that until there is one, because the
	// organization is created by a command outside this process.
	notifier := notify.New(system, notify.NewSender(cfg.Notify.Timeout, nil), cfg.Notify.Batch, log)
	go notify.NewLoop(notifier, cfg.Notify.Interval, cfg.Notify.UnobservableAlert, notify.ConfigChannel{
		URL:         cfg.Notify.WebhookURL,
		Template:    cfg.Notify.Template,
		MinPriority: cfg.Notify.MinPriority,
	}).Run(ctx)

	// The browser loop, and the one thing here that is optional. A deployment
	// with no rendering service is a deployment that only probes: the assets
	// keep their due dates and nothing pretends to have looked at them, which
	// is the honest shape rather than a loop failing every minute.
	if cfg.Render.URL != "" {
		budget := render.NewBudget(cfg.Render.Cost, nil)
		client := fingerprint.New(cfg.Render.URL, cfg.Render.Timeout, nil)
		pass := render.New(system, client, ingestor, budget, render.Options{
			Batch:             cfg.Render.Batch,
			Concurrency:       cfg.Render.Concurrency,
			UnobservableAlert: cfg.Render.UnobservableAlert,
		}, log)
		go render.NewLoop(pass, cfg.Render.Interval).Run(ctx)
		log.InfoContext(ctx, "rendering", "service", cfg.Render.URL, "cost", budget.Cost())
	} else {
		log.WarnContext(ctx, "no rendering service configured, nothing will be rendered")
	}

	// The Certificate Transparency matcher, optional for the same reason the
	// browser is. A deployment with no feed configured still walks every
	// perimeter on the discovery cadence; what it gives up is the freshness
	// advantage, and saying so is better than a loop dialling nothing.
	if cfg.CT.URL != "" {
		matcher := ct.New(system, scoped, ingestor, ct.Options{
			Interval:  cfg.CT.Interval,
			Ceiling:   cfg.CT.Ceiling,
			Window:    cfg.CT.Window,
			CacheTTL:  cfg.CT.CacheTTL,
			CacheSize: cfg.CT.CacheSize,
		}, log)
		go ct.NewLoop(matcher).Run(ctx)
		go ct.NewFeed(cfg.CT.URL, matcher, log).Run(ctx)
		log.InfoContext(ctx, "watching certificate transparency", "feed", cfg.CT.URL)
	} else {
		log.WarnContext(ctx, "no certificate transparency feed configured, "+
			"discovery runs on its cadence alone")
	}

	srv := &http.Server{
		Addr:              cfg.HTTP.Addr,
		Handler:           routes(cfg, scoped, system, signer, scheduler, ingestor, enricher.Configured(), log),
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

// startPlatform is what actually starts a run definition.
//
// A deployment with none defines runs and starts nothing, which is the
// development shape and a legitimate one: the console shows the definition and
// a person runs the image.
func startPlatform(cfg *config.Config, log *slog.Logger) (runs.Platform, error) {
	switch cfg.Runner.Provider {
	case config.RunnerScaleway:
		log.Info("runs are started on scaleway", "region", cfg.Runner.Region, "job", cfg.Runner.JobID)
		return runner.NewScaleway(cfg.Runner.Endpoint, cfg.Runner.Region, cfg.Runner.JobID,
			cfg.Runner.SecretKey, cfg.Runner.Timeout, log), nil
	case config.RunnerNone:
		log.Warn("no platform configured, run definitions are rendered and started by hand")
		return runner.NewNone(log), nil
	default:
		return nil, fmt.Errorf("unknown runner %q", cfg.Runner.Provider)
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
	cfg *config.Config, scoped *store.Scoped, system *pgxpool.Pool, signer *auth.Signer,
	scheduler *runs.Scheduler, ingestor *ingest.Ingestor, enriched bool, log *slog.Logger,
) http.Handler {
	mux := http.NewServeMux()

	// Where a run's report lands. It authenticates before reading anything
	// else, and the run comes from the credential rather than from the body.
	mux.Handle("POST /reports", api.NewReports(scoped, system, signer, ingestor, log))

	// The only thing a run is given about the inventory, scoped to that run.
	mux.Handle("GET /runs/{run}/targets", api.NewTargets(scoped, system, signer, log))

	// The console surface. Every route goes through one authorization layer
	// that produces a principal, even while there is one kind of caller.
	guard := api.NewGuard(system, log)
	programs := api.NewPrograms(scoped, scheduler, ingestor, cfg.Runner.Timeout, log)
	mux.Handle("POST /programs/{program}/runs", guard.Require(auth.ActionManageJobs, programs.StartRun))
	// Entering an asset by hand is an assertion about the perimeter, which is
	// a different privilege from writing what a scanner found.
	mux.Handle("POST /programs/{program}/assets", guard.Require(auth.ActionManageScope, programs.EnterAssets))

	// The two render triggers that are API entry points. They hold manage_jobs
	// rather than ingest: something holding ingest could otherwise schedule
	// renders of its choosing and spend a programme's budget on targets it
	// picked.
	renders := api.NewRenders(scoped, cfg.Render.ReplanSpread, log)
	mux.Handle("POST /assets/{asset}/render", guard.Require(auth.ActionManageJobs, renders.Request))
	mux.Handle("POST /renders/replan", guard.Require(auth.ActionManageJobs, renders.Replan))

	// The inventory itself. read_assets and never ingest: a run holds ingest
	// and everything it needs is in its definition, so a compromised one cannot
	// exfiltrate a perimeter.
	assets := api.NewAssets(scoped, enriched, log)
	mux.Handle("POST /assets/search", guard.Require(auth.ActionReadAssets, assets.Search))
	// The folded list, which is the one the console renders. A route of its own
	// rather than a flag, because the two hand out cursors that mean different
	// columns and swapping them has to be a refusal.
	mux.Handle("POST /assets/hosts", guard.Require(auth.ActionReadAssets, assets.Hosts))
	mux.Handle("POST /assets/facets", guard.Require(auth.ActionReadAssets, assets.Facets))
	mux.Handle("POST /assets/export", guard.Require(auth.ActionReadAssets, assets.Export))
	// The one read path that touches the journal, on one asset and on demand.
	mux.Handle("GET /assets/{asset}", guard.Require(auth.ActionReadAssets, assets.Get))
	// What the search accepts and what this deployment can do, served rather
	// than deduced. A console that learns its vocabulary against 400s learns it
	// wrong, and one that deduces the enrichment state from missing data
	// guesses.
	mux.Handle("GET /assets/fields", guard.Require(auth.ActionReadAssets, assets.Fields))

	// The live feed. Polling with a cursor, so nothing pins a connection
	// between rounds and the tenant filter is the one every other read uses.
	feed := api.NewFeed(scoped, log)
	mux.Handle("GET /feed", guard.Require(auth.ActionReadAssets, feed.Stream))

	// Perimeters, rules and the queue. The reads are the inventory's action and
	// the writes are the scope one: entering a perimeter is an assertion about
	// what may be scanned, which is a different privilege from reading what was
	// found.
	console := api.NewConsole(scoped, ingestor, log)
	mux.Handle("GET /programs", guard.Require(auth.ActionReadAssets, console.ListPrograms))
	mux.Handle("GET /programs/{program}", guard.Require(auth.ActionReadAssets, console.GetProgram))
	mux.Handle("POST /programs", guard.Require(auth.ActionManageScope, console.CreateProgram))
	mux.Handle("PATCH /programs/{program}", guard.Require(auth.ActionManageScope, console.UpdateProgram))
	// The rule routes are nested and not reachable on their own. A rule
	// identifier alone would need its own ownership check, and a second place
	// deciding who may touch what is a second place to get it wrong.
	mux.Handle("POST /programs/{program}/rules", guard.Require(auth.ActionManageScope, console.CreateRule))
	mux.Handle("PATCH /programs/{program}/rules/{rule}", guard.Require(auth.ActionManageScope, console.UpdateRule))
	// Read from a console, never from what consumes it.
	mux.Handle("GET /queue", guard.Require(auth.ActionReadAssets, console.Queue))

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

		// Both pools, because a request needs both. The system one is what
		// turns a credential into an organization, so a broken one 500s every
		// authenticated call while a process pinging only the other keeps
		// reporting itself ready and stays in the rotation.
		for name, ping := range map[string]func(context.Context) error{
			"database":        scoped.Ping,
			"system_database": system.Ping,
		} {
			if err := ping(ctx); err != nil {
				log.ErrorContext(ctx, "readiness check failed", "pool", name, "error", err)
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{
					"status": "unavailable",
					"reason": name,
				})
				return
			}
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
