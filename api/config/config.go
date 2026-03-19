package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

type Config struct {
	// Server
	Port        string
	APIBaseURL  string
	CORSOrigins []string

	// Database
	DatabaseURL string

	// Auth
	JWTSecret    string
	JWTExpiry    time.Duration
	PasswordHash string
	IngestAPIKey string

	// Fingerprinter
	FingerprinterURL string
	FingerprintWorkers int

	// Scaleway
	ScalewayAccessKey string
	ScalewaySecretKey string
	ScalewayProjectID string
	ScalewayRegion          string
	ScalewayJobDefinitionID string

	// Scheduler
	ReconCron        string
	RevalidationCron string

	// Notifications
	DiscordWebhookURL          string
	NotificationCallbackURL    string
	NotificationCallbackSecret string
}

func Load() (*Config, error) {
	// Load .env if present, ignore if missing
	_ = godotenv.Load()

	cfg := &Config{}

	// Required
	var missing []string

	cfg.DatabaseURL = os.Getenv("DATABASE_URL")
	if cfg.DatabaseURL == "" {
		missing = append(missing, "DATABASE_URL")
	}

	cfg.JWTSecret = os.Getenv("JWT_SECRET")
	if cfg.JWTSecret == "" {
		missing = append(missing, "JWT_SECRET")
	}

	cfg.PasswordHash = os.Getenv("PASSWORD_HASH")
	if cfg.PasswordHash == "" {
		missing = append(missing, "PASSWORD_HASH")
	}

	cfg.IngestAPIKey = os.Getenv("INGEST_API_KEY")
	if cfg.IngestAPIKey == "" {
		missing = append(missing, "INGEST_API_KEY")
	}

	cfg.FingerprinterURL = os.Getenv("FINGERPRINTER_URL")
	if cfg.FingerprinterURL == "" {
		missing = append(missing, "FINGERPRINTER_URL")
	}

	cfg.ScalewayAccessKey = os.Getenv("SCALEWAY_ACCESS_KEY")
	if cfg.ScalewayAccessKey == "" {
		missing = append(missing, "SCALEWAY_ACCESS_KEY")
	}

	cfg.ScalewaySecretKey = os.Getenv("SCALEWAY_SECRET_KEY")
	if cfg.ScalewaySecretKey == "" {
		missing = append(missing, "SCALEWAY_SECRET_KEY")
	}

	cfg.ScalewayProjectID = os.Getenv("SCALEWAY_PROJECT_ID")
	if cfg.ScalewayProjectID == "" {
		missing = append(missing, "SCALEWAY_PROJECT_ID")
	}

	cfg.ScalewayJobDefinitionID = os.Getenv("SCALEWAY_JOB_DEFINITION_ID")
	if cfg.ScalewayJobDefinitionID == "" {
		missing = append(missing, "SCALEWAY_JOB_DEFINITION_ID")
	}

	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required environment variables: %v", missing)
	}

	// Optional with defaults
	cfg.Port = os.Getenv("PORT")
	if cfg.Port == "" {
		cfg.Port = "3002"
	}

	jwtExpiry := os.Getenv("JWT_EXPIRY")
	if jwtExpiry == "" {
		jwtExpiry = "24h"
	}
	dur, err := time.ParseDuration(jwtExpiry)
	if err != nil {
		return nil, fmt.Errorf("invalid JWT_EXPIRY %q: %w", jwtExpiry, err)
	}
	cfg.JWTExpiry = dur

	cfg.ScalewayRegion = os.Getenv("SCALEWAY_REGION")
	if cfg.ScalewayRegion == "" {
		cfg.ScalewayRegion = "fr-par"
	}

	cfg.ReconCron = os.Getenv("RECON_CRON")
	if cfg.ReconCron == "" {
		cfg.ReconCron = "0 2 * * 1"
	}

	cfg.RevalidationCron = os.Getenv("REVALIDATION_CRON")
	if cfg.RevalidationCron == "" {
		cfg.RevalidationCron = "0 2 * * 4"
	}

	workersStr := os.Getenv("FINGERPRINT_WORKERS")
	if workersStr == "" {
		cfg.FingerprintWorkers = 3
	} else {
		w, err := strconv.Atoi(workersStr)
		if err != nil || w < 1 {
			return nil, fmt.Errorf("invalid FINGERPRINT_WORKERS %q: must be a positive integer", workersStr)
		}
		cfg.FingerprintWorkers = w
	}

	cfg.APIBaseURL = os.Getenv("API_BASE_URL")

	corsOrigins := os.Getenv("CORS_ORIGINS")
	if corsOrigins == "" {
		cfg.CORSOrigins = []string{"http://localhost:3000"}
	} else {
		cfg.CORSOrigins = strings.Split(corsOrigins, ",")
	}

	// Optional (no default, no-op if empty)
	cfg.DiscordWebhookURL = os.Getenv("DISCORD_WEBHOOK_URL")
	cfg.NotificationCallbackURL = os.Getenv("NOTIFICATION_CALLBACK_URL")
	cfg.NotificationCallbackSecret = os.Getenv("NOTIFICATION_CALLBACK_SECRET")

	return cfg, nil
}
