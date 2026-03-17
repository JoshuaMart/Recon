package scheduler

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/go-co-op/gocron/v2"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jomar/recon/api/db"
	"github.com/jomar/recon/api/handler"
)

type DigestStats struct {
	WildcardsProcessed int
	NewHostnames       int
	NewlyDead          int
	NewWebServices     int
	Duration           time.Duration
}

type DigestFunc func(ctx context.Context, kind string, stats DigestStats)

type RevalidateFunc func(ctx context.Context, wildcard db.Wildcard) DigestStats

type Scheduler struct {
	queries    *db.Queries
	launcher   handler.JobLauncher
	revalidate RevalidateFunc
	onDigest   DigestFunc
	mu         sync.Mutex
}

func New(queries *db.Queries, launcher handler.JobLauncher, revalidate RevalidateFunc, onDigest DigestFunc) *Scheduler {
	return &Scheduler{
		queries:    queries,
		launcher:   launcher,
		revalidate: revalidate,
		onDigest:   onDigest,
	}
}

func (s *Scheduler) Start(reconCron, revalidationCron string) error {
	sched, err := gocron.NewScheduler()
	if err != nil {
		return fmt.Errorf("failed to create scheduler: %w", err)
	}

	_, err = sched.NewJob(
		gocron.CronJob(reconCron, false),
		gocron.NewTask(s.runRecon),
	)
	if err != nil {
		return fmt.Errorf("failed to schedule recon job: %w", err)
	}

	_, err = sched.NewJob(
		gocron.CronJob(revalidationCron, false),
		gocron.NewTask(s.runRevalidation),
	)
	if err != nil {
		return fmt.Errorf("failed to schedule revalidation job: %w", err)
	}

	sched.Start()
	log.Printf("Scheduler started (recon=%q, revalidation=%q)", reconCron, revalidationCron)
	return nil
}

func (s *Scheduler) runRecon() {
	// Mutual exclusion with revalidation
	if !s.mu.TryLock() {
		log.Println("Scheduler: recon skipped, another task is running")
		return
	}
	defer s.mu.Unlock()

	ctx := context.Background()
	start := time.Now()
	log.Println("Scheduler: starting periodic recon")

	wildcards, err := s.queries.GetActiveWildcards(ctx)
	if err != nil {
		log.Printf("Scheduler: failed to get active wildcards: %v", err)
		return
	}

	launched := 0
	for _, wc := range wildcards {
		// Check concurrency limit
		activeCount, err := s.queries.CountActiveJobs(ctx)
		if err != nil {
			log.Printf("Scheduler: failed to count active jobs: %v", err)
			continue
		}
		if activeCount >= 10 {
			log.Printf("Scheduler: concurrency limit reached, skipping remaining wildcards")
			break
		}

		// Check no active job for this wildcard
		hasActive, err := s.queries.HasActiveJobForWildcard(ctx, wc.ID)
		if err != nil || hasActive {
			continue
		}

		// Create job record
		job, err := s.queries.InsertJob(ctx, db.InsertJobParams{
			WildcardID: wc.ID,
			Mode:       db.ReconModeNormal,
		})
		if err != nil {
			log.Printf("Scheduler: failed to create job for %s: %v", wc.Value, err)
			continue
		}

		// Launch via Scaleway
		if s.launcher != nil {
			jobID := uuidToString(job.ID)
			_, err := s.launcher.LaunchJob(wc.Value, jobID, "normal")
			if err != nil {
				log.Printf("Scheduler: failed to launch job for %s: %v", wc.Value, err)
				_ = s.queries.UpdateJobStatus(ctx, db.UpdateJobStatusParams{
					ID:     job.ID,
					Status: db.JobStatusFailed,
				})
				continue
			}
		}

		launched++
	}

	duration := time.Since(start)
	log.Printf("Scheduler: recon completed, launched %d jobs in %s", launched, duration)

	if s.onDigest != nil {
		s.onDigest(ctx, "recon", DigestStats{
			WildcardsProcessed: len(wildcards),
			Duration:           duration,
		})
	}
}

func (s *Scheduler) runRevalidation() {
	// Mutual exclusion with recon
	if !s.mu.TryLock() {
		log.Println("Scheduler: revalidation skipped, another task is running")
		return
	}
	defer s.mu.Unlock()

	ctx := context.Background()
	start := time.Now()
	log.Println("Scheduler: starting periodic revalidation")

	wildcards, err := s.queries.GetActiveWildcards(ctx)
	if err != nil {
		log.Printf("Scheduler: failed to get active wildcards: %v", err)
		return
	}

	totalStats := DigestStats{WildcardsProcessed: len(wildcards)}

	for _, wc := range wildcards {
		if s.revalidate != nil {
			wcStats := s.revalidate(ctx, wc)
			totalStats.NewHostnames += wcStats.NewHostnames
			totalStats.NewlyDead += wcStats.NewlyDead
			totalStats.NewWebServices += wcStats.NewWebServices
		}
		_ = s.queries.UpdateLastRevalidatedAt(ctx, wc.ID)
	}

	totalStats.Duration = time.Since(start)
	log.Printf("Scheduler: revalidation completed in %s", totalStats.Duration)

	if s.onDigest != nil {
		s.onDigest(ctx, "revalidation", totalStats)
	}
}

func uuidToString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	b := u.Bytes
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
