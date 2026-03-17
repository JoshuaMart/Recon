package revalidation

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"time"

	"github.com/jomar/recon/api/db"
	"github.com/jomar/recon/api/scheduler"
)

type Service struct {
	queries *db.Queries
}

func New(queries *db.Queries) *Service {
	return &Service{queries: queries}
}

func (s *Service) RevalidateWildcard(ctx context.Context, wc db.Wildcard) scheduler.DigestStats {
	stats := scheduler.DigestStats{}

	hostnames, err := s.queries.ListHostnamesByWildcard(ctx, wc.ID)
	if err != nil {
		log.Printf("Revalidation: failed to list hostnames for %s: %v", wc.Value, err)
		return stats
	}

	for _, h := range hostnames {
		result := s.revalidateHostname(ctx, h)
		stats.NewlyDead += result.newlyDead
		stats.NewWebServices += result.webEnqueued
	}

	return stats
}

type hostnameResult struct {
	newlyDead   int
	webEnqueued int
}

func (s *Service) revalidateHostname(ctx context.Context, h db.Hostname) hostnameResult {
	result := hostnameResult{}

	// Step 1: DNS resolution
	ips, err := net.DefaultResolver.LookupHost(ctx, h.Fqdn)
	if err != nil || len(ips) == 0 {
		// DNS failed
		if h.Status == db.HostnameStatusAlive {
			_ = s.queries.UpdateHostnameStatus(ctx, db.UpdateHostnameStatusParams{
				ID:     h.ID,
				Status: db.HostnameStatusDead,
			})
			result.newlyDead = 1
		}
		return result
	}

	// Step 2: DNS resolves — check by type
	switch h.Type {
	case db.HostnameTypeWeb:
		// Re-fingerprint known URLs
		webResults, err := s.getWebURLsForHostname(ctx, h)
		if err != nil {
			break
		}
		for _, url := range webResults {
			_, _ = s.queries.EnqueueFingerprint(ctx, db.EnqueueFingerprintParams{
				Url:        url,
				HostnameID: h.ID,
				Source:     "revalidation",
			})
			result.webEnqueued++
		}
		// Mark alive + update last_seen
		_ = s.queries.UpdateHostnameLastSeen(ctx, h.ID)

	case db.HostnameTypeOther:
		// TCP dial on known open ports
		ports := extractOpenPorts(h.Ports)
		anyResponded := false
		for _, port := range ports {
			if tcpCheck(h.Fqdn, port) {
				anyResponded = true
				break
			}
		}
		if anyResponded {
			_ = s.queries.UpdateHostnameLastSeen(ctx, h.ID)
		} else if h.Status == db.HostnameStatusAlive {
			_ = s.queries.UpdateHostnameStatus(ctx, db.UpdateHostnameStatusParams{
				ID:     h.ID,
				Status: db.HostnameStatusDead,
			})
			result.newlyDead = 1
		}

	default:
		// unknown type — just mark alive since DNS resolves
		_ = s.queries.UpdateHostnameLastSeen(ctx, h.ID)
	}

	return result
}

func (s *Service) getWebURLsForHostname(ctx context.Context, h db.Hostname) ([]string, error) {
	// Get URLs from web_results for this hostname
	rows, err := s.queries.ListWebResults(ctx, db.ListWebResultsParams{
		HostnameID: h.ID,
		Limit:      1000,
		Offset:     0,
	})
	if err != nil {
		return nil, err
	}

	urls := make([]string, 0, len(rows))
	for _, row := range rows {
		urls = append(urls, row.Url)
	}
	return urls, nil
}

func extractOpenPorts(portsJSON []byte) []string {
	if len(portsJSON) == 0 {
		return nil
	}

	var parsed struct {
		TCP map[string]json.RawMessage `json:"tcp"`
	}
	if err := json.Unmarshal(portsJSON, &parsed); err != nil {
		return nil
	}

	ports := make([]string, 0, len(parsed.TCP))
	for port := range parsed.TCP {
		ports = append(ports, port)
	}
	return ports
}

func tcpCheck(host, port string) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), 5*time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

