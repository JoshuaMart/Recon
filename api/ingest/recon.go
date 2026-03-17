package ingest

import (
	"encoding/json"
	"net/http"
	"net/netip"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jomar/recon/api/db"
)

type ReconHandler struct {
	queries *db.Queries
}

func NewReconHandler(queries *db.Queries) *ReconHandler {
	return &ReconHandler{queries: queries}
}

type reconRequest struct {
	JobID string    `json:"job_id"`
	Host  reconHost `json:"host"`
}

type reconHost struct {
	FQDN  string          `json:"fqdn"`
	IP    *string         `json:"ip"`
	CDN   *string         `json:"cdn"`
	DNS   json.RawMessage `json:"dns"`
	Ports json.RawMessage `json:"ports"`
}

func (h *ReconHandler) Handle(w http.ResponseWriter, r *http.Request) {
	var req reconRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	if req.Host.FQDN == "" {
		http.Error(w, `{"error":"missing host.fqdn"}`, http.StatusBadRequest)
		return
	}

	var jobID pgtype.UUID
	if err := jobID.Scan(req.JobID); err != nil {
		http.Error(w, `{"error":"invalid job_id"}`, http.StatusBadRequest)
		return
	}

	job, err := h.queries.GetJob(r.Context(), jobID)
	if err != nil {
		http.Error(w, `{"error":"job not found"}`, http.StatusNotFound)
		return
	}

	// Determine status based on IP
	status := db.HostnameStatusAlive
	if req.Host.IP == nil {
		status = db.HostnameStatusUnreachable
	}

	// Determine type based on ports
	hostType := db.HostnameTypeUnknown
	if hasWebService(req.Host.Ports) {
		hostType = db.HostnameTypeWeb
	} else if hasPorts(req.Host.Ports) {
		hostType = db.HostnameTypeOther
	}

	// Parse IP
	var ip *netip.Addr
	if req.Host.IP != nil {
		parsed, err := netip.ParseAddr(*req.Host.IP)
		if err == nil {
			ip = &parsed
		}
	}

	// Upsert hostname
	now := pgtype.Timestamptz{Time: time.Now(), Valid: true}
	var lastSeen pgtype.Timestamptz
	if status == db.HostnameStatusAlive {
		lastSeen = now
	}

	hostname, err := h.queries.UpsertHostname(r.Context(), db.UpsertHostnameParams{
		WildcardID: job.WildcardID,
		Fqdn:       req.Host.FQDN,
		Ip:         ip,
		Cdn:        textFromPtr(req.Host.CDN),
		Status:     status,
		Type:       hostType,
		Dns:        req.Host.DNS,
		Ports:      req.Host.Ports,
		LastSeenAt: lastSeen,
	})
	if err != nil {
		http.Error(w, `{"error":"failed to upsert hostname"}`, http.StatusInternalServerError)
		return
	}

	// Enqueue web URLs for fingerprinting
	urls := extractWebURLs(req.Host.Ports)
	for _, u := range urls {
		_, _ = h.queries.EnqueueFingerprint(r.Context(), db.EnqueueFingerprintParams{
			Url:        u,
			HostnameID: hostname.ID,
			Source:     "recon",
		})
	}

	w.WriteHeader(http.StatusOK)
}

func hasWebService(ports json.RawMessage) bool {
	return len(extractWebURLs(ports)) > 0
}

func hasPorts(ports json.RawMessage) bool {
	if len(ports) == 0 || string(ports) == "null" {
		return false
	}
	var parsed struct {
		TCP map[string]json.RawMessage `json:"tcp"`
		UDP map[string]json.RawMessage `json:"udp"`
	}
	if err := json.Unmarshal(ports, &parsed); err != nil {
		return false
	}
	return len(parsed.TCP) > 0 || len(parsed.UDP) > 0
}

func extractWebURLs(ports json.RawMessage) []string {
	if len(ports) == 0 {
		return nil
	}

	var parsed struct {
		TCP map[string]struct {
			Web *string `json:"web"`
		} `json:"tcp"`
		UDP map[string]struct {
			Web *string `json:"web"`
		} `json:"udp"`
	}
	if err := json.Unmarshal(ports, &parsed); err != nil {
		return nil
	}

	var urls []string
	for _, p := range parsed.TCP {
		if p.Web != nil {
			urls = append(urls, *p.Web)
		}
	}
	for _, p := range parsed.UDP {
		if p.Web != nil {
			urls = append(urls, *p.Web)
		}
	}
	return urls
}

func textFromPtr(s *string) pgtype.Text {
	if s == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *s, Valid: true}
}
