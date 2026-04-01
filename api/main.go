package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jomar/recon/api/cmd"
	"github.com/jomar/recon/api/config"
	"github.com/jomar/recon/api/db"
	"github.com/jomar/recon/api/fingerprint"
	"github.com/jomar/recon/api/handler"
	"github.com/jomar/recon/api/ingest"
	"github.com/jomar/recon/api/middleware"
	"github.com/jomar/recon/api/notify"
	"github.com/jomar/recon/api/revalidation"
	"github.com/jomar/recon/api/scaleway"
	"github.com/jomar/recon/api/scheduler"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "hash-password" {
		cmd.HashPassword()
		return
	}

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Database
	pool, err := pgxpool.New(context.Background(), cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}

	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		log.Fatalf("Failed to ping database: %v", err)
	}
	log.Println("Connected to database")

	// Queries
	queries := db.New(pool)

	// Scaleway client
	scwClient, err := scaleway.NewClient(
		cfg.ScalewayAccessKey, cfg.ScalewaySecretKey, cfg.ScalewayProjectID,
		cfg.ScalewayRegion, cfg.ScalewayJobDefinitionID, cfg.APIBaseURL, cfg.IngestAPIKey,
	)
	if err != nil {
		log.Fatalf("Failed to create Scaleway client: %v", err)
	}

	// Notifications
	notifier := notify.NewNotifier(cfg.DiscordWebhookURL, cfg.NotificationCallbackURL, cfg.NotificationCallbackSecret)

	// Revalidation
	revalService := revalidation.New(queries)

	// Handlers
	authHandler := handler.NewAuthHandler(cfg.PasswordHash, cfg.JWTSecret, cfg.JWTExpiry)
	wildcardHandler := handler.NewWildcardHandler(queries, scwClient, func(ctx context.Context, wc db.Wildcard) {
		revalService.RevalidateWildcard(ctx, wc)
	})
	hostnameHandler := handler.NewHostnameHandler(queries)
	urlHandler := handler.NewURLHandler(queries)
	jobHandler := handler.NewJobHandler(queries)
	reconIngest := ingest.NewReconHandler(queries)
	certstreamIngest := ingest.NewCertstreamHandler(queries)
	jobStatusIngest := ingest.NewJobStatusHandler(queries, notifier)

	// Fingerprint worker
	fpClient := fingerprint.NewClient(cfg.FingerprinterURL)
	fpWorker := fingerprint.NewWorker(queries, fpClient, func(ctx context.Context, hostname db.Hostname, result *fingerprint.Result) {
		// Resolve wildcard value for notification
		wc, err := queries.GetWildcard(ctx, hostname.WildcardID)
		if err != nil {
			return
		}
		hostnameID := fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
			hostname.ID.Bytes[0:4], hostname.ID.Bytes[4:6], hostname.ID.Bytes[6:8], hostname.ID.Bytes[8:10], hostname.ID.Bytes[10:16])

		discordInfo, callbackPayload := notify.BuildWebServiceNotification(
			result.URL, wc.Value, hostnameID,
			result.Chain, result.Technologies, result.Metadata,
		)
		notifier.NotifyNewWebService(discordInfo, callbackPayload)
	})
	fpWorker.Start(context.Background(), cfg.FingerprintWorkers)

	// Scheduler
	sched := scheduler.New(queries, scwClient, scwClient, revalService.RevalidateWildcard, func(_ context.Context, kind string, stats scheduler.DigestStats) {
		notifier.NotifyDigest(kind, notify.DigestInfo{
			Kind:               kind,
			WildcardsProcessed: stats.WildcardsProcessed,
			NewHostnames:       stats.NewHostnames,
			NewlyDead:          stats.NewlyDead,
			NewWebServices:     stats.NewWebServices,
			Duration:           stats.Duration,
		})
	})
	if err := sched.Start(cfg.ReconCron, cfg.RevalidationCron); err != nil {
		log.Fatalf("Failed to start scheduler: %v", err)
	}

	// Router
	r := chi.NewRouter()
	r.Use(chimw.Logger)
	r.Use(chimw.Recoverer)
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   cfg.CORSOrigins,
		AllowedMethods:   []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-API-Key"},
		ExposedHeaders:   []string{"Link"},
		AllowCredentials: true,
		MaxAge:           300,
	}))

	r.Get("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	r.Route("/api", func(r chi.Router) {
		// Auth (public)
		r.Route("/auth", func(r chi.Router) {
			r.Post("/login", authHandler.Login)
			r.Post("/refresh", authHandler.Refresh)
		})

		// Ingest (API key protected)
		r.Route("/ingest", func(r chi.Router) {
			r.Use(middleware.APIKeyAuth(cfg.IngestAPIKey))
			r.Post("/recon", reconIngest.Handle)
			r.Post("/certstream", certstreamIngest.Handle)
		})

		// Job status update (API key protected, called by recon job)
		r.With(middleware.APIKeyAuth(cfg.IngestAPIKey)).Patch("/jobs/{id}/status", jobStatusIngest.Handle)

		// Protected routes (JWT)
		r.Group(func(r chi.Router) {
			r.Use(middleware.JWTAuth(cfg.JWTSecret))

			r.Get("/wildcards", wildcardHandler.List)
			r.Post("/wildcards", wildcardHandler.Create)
			r.Delete("/wildcards/{id}", wildcardHandler.Delete)
			r.Post("/wildcards/{id}/recon", wildcardHandler.LaunchRecon)
			r.Put("/wildcards/{id}/revalidate", wildcardHandler.Revalidate)

			r.Get("/hostnames", hostnameHandler.List)
			r.Get("/hostnames/{id}", hostnameHandler.Get)

			r.Get("/urls", urlHandler.List)
			r.Get("/urls/{id}", urlHandler.Get)
			r.Post("/urls/fingerprint", urlHandler.Fingerprint)

			r.Get("/jobs", jobHandler.List)
			r.Get("/jobs/{id}", jobHandler.Get)
		})
	})

	addr := fmt.Sprintf(":%s", cfg.Port)
	log.Printf("Starting API server on %s", addr)

	log.Fatal(http.ListenAndServe(addr, r))
}
