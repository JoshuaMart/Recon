package fingerprint

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jomar/recon/api/db"
)

type NotifyFunc func(ctx context.Context, hostname db.Hostname, result *Result)

type Worker struct {
	queries  *db.Queries
	client   *Client
	onNewWeb NotifyFunc
}

func NewWorker(queries *db.Queries, client *Client, onNewWeb NotifyFunc) *Worker {
	return &Worker{
		queries:  queries,
		client:   client,
		onNewWeb: onNewWeb,
	}
}

func (w *Worker) Start(ctx context.Context, workers int) {
	for i := range workers {
		go w.run(ctx, i)
	}
	go w.maintenance(ctx)
}

// maintenance periodically recovers stale "processing" items (orphaned by crashed workers).
func (w *Worker) maintenance(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := w.queries.RecoverStaleProcessing(ctx); err != nil {
				log.Printf("Failed to recover stale processing items: %v", err)
			}
		}
	}
}

func (w *Worker) run(ctx context.Context, id int) {
	log.Printf("Fingerprint worker %d started", id)
	for {
		select {
		case <-ctx.Done():
			log.Printf("Fingerprint worker %d stopped", id)
			return
		default:
			if !w.processNext(ctx) {
				// No items, wait before polling again
				select {
				case <-ctx.Done():
					return
				case <-time.After(2 * time.Second):
				}
			}
		}
	}
}

func (w *Worker) processNext(ctx context.Context) bool {
	item, err := w.queries.DequeueFingerprint(ctx)
	if err != nil {
		// No pending items
		return false
	}

	log.Printf("Fingerprinting %s (source=%s)", item.Url, item.Source)

	// Get current hostname state before fingerprinting
	hostname, err := w.queries.GetHostname(ctx, item.HostnameID)
	if err != nil {
		log.Printf("Failed to get hostname for queue item %s: %v", uuidStr(item.ID), err)
		_ = w.queries.MarkFingerprintFailed(ctx, item.ID)
		return true
	}

	wasNew := hostname.Status == db.HostnameStatusUnreachable

	result, err := w.client.Scan(item.Url)
	if err != nil {
		log.Printf("Fingerprint failed for %s: %v", item.Url, err)
		_ = w.queries.MarkFingerprintFailed(ctx, item.ID)

		// Update hostname status on failure
		if hostname.Status == db.HostnameStatusAlive {
			_ = w.queries.UpdateHostnameStatus(ctx, db.UpdateHostnameStatusParams{
				ID:     item.HostnameID,
				Status: db.HostnameStatusDead,
			})
		}
		return true
	}

	// Upsert web result
	scannedAt := pgtype.Timestamptz{Time: result.ScannedAt, Valid: !result.ScannedAt.IsZero()}
	_, err = w.queries.UpsertWebResult(ctx, db.UpsertWebResultParams{
		HostnameID:    item.HostnameID,
		Url:           item.Url,
		Chain:         result.Chain,
		Technologies:  result.Technologies,
		Cookies:       result.Cookies,
		Metadata:      result.Metadata,
		ExternalHosts: result.ExternalHosts,
		ScannedAt:     scannedAt,
	})
	if err != nil {
		log.Printf("Failed to upsert web result for %s: %v", item.Url, err)
		_ = w.queries.MarkFingerprintFailed(ctx, item.ID)
		return true
	}

	// Update hostname: alive + web
	_ = w.queries.UpdateHostnameStatus(ctx, db.UpdateHostnameStatusParams{
		ID:     item.HostnameID,
		Status: db.HostnameStatusAlive,
	})
	_ = w.queries.UpdateHostnameLastSeen(ctx, item.HostnameID)

	_ = w.queries.MarkFingerprintDone(ctx, item.ID)

	// Notify if certstream source and hostname was new
	if item.Source == "certstream" && wasNew && w.onNewWeb != nil {
		// Refetch hostname with updated state
		updated, err := w.queries.GetHostname(ctx, item.HostnameID)
		if err == nil {
			w.onNewWeb(ctx, updated, result)
		}
	}

	log.Printf("Fingerprint done for %s", item.Url)
	return true
}

func uuidStr(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	b := u.Bytes
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
