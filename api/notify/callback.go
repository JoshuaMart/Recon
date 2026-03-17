package notify

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"time"
)

type CallbackClient struct {
	callbackURL string
	secret      string
	httpClient  *http.Client
}

func NewCallbackClient(callbackURL, secret string) *CallbackClient {
	return &CallbackClient{
		callbackURL: callbackURL,
		secret:      secret,
		httpClient:  &http.Client{Timeout: 10 * time.Second},
	}
}

type NewWebServicePayload struct {
	Event        string               `json:"event"`
	URL          string               `json:"url"`
	Wildcard     string               `json:"wildcard"`
	HostnameID   string               `json:"hostname_id"`
	Fingerprint  FingerprintSummary   `json:"fingerprint"`
	DiscoveredAt string               `json:"discovered_at"`
}

type FingerprintSummary struct {
	StatusCode   int              `json:"status_code"`
	Title        string           `json:"title"`
	Technologies []TechInfo       `json:"technologies"`
	Favicon      string           `json:"favicon,omitempty"`
}

type TechInfo struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	Category string `json:"category"`
}

func (c *CallbackClient) SendNewWebService(payload NewWebServicePayload) {
	if c.callbackURL == "" {
		return
	}
	payload.Event = "new_web_service"
	c.send(payload)
}

type DigestPayload struct {
	Event              string `json:"event"`
	WildcardsProcessed int    `json:"wildcards_processed"`
	NewHostnames       int    `json:"new_hostnames"`
	NewlyDead          int    `json:"newly_dead"`
	NewWebServices     int    `json:"new_web_services"`
	DurationSeconds    int    `json:"duration_seconds"`
	CompletedAt        string `json:"completed_at"`
}

func (c *CallbackClient) SendDigest(kind string, info DigestInfo) {
	if c.callbackURL == "" {
		return
	}

	event := "recon_digest"
	if kind == "revalidation" {
		event = "revalidation_digest"
	}

	payload := DigestPayload{
		Event:              event,
		WildcardsProcessed: info.WildcardsProcessed,
		NewHostnames:       info.NewHostnames,
		NewlyDead:          info.NewlyDead,
		NewWebServices:     info.NewWebServices,
		DurationSeconds:    int(info.Duration.Seconds()),
		CompletedAt:        time.Now().UTC().Format(time.RFC3339),
	}

	c.send(payload)
}

func (c *CallbackClient) send(payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		log.Printf("Callback: failed to marshal payload: %v", err)
		return
	}

	req, err := http.NewRequest(http.MethodPost, c.callbackURL, bytes.NewReader(body))
	if err != nil {
		log.Printf("Callback: failed to create request: %v", err)
		return
	}

	req.Header.Set("Content-Type", "application/json")
	if c.secret != "" {
		req.Header.Set("X-API-Key", c.secret)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		log.Printf("Callback: failed to send to %s: %v", c.callbackURL, err)
		return
	}
	_ = resp.Body.Close()

	if resp.StatusCode >= 300 {
		log.Printf("Callback: %s returned status %d", c.callbackURL, resp.StatusCode)
	}
}

// Notifier aggregates Discord and Callback notifications.
type Notifier struct {
	Discord  *DiscordClient
	Callback *CallbackClient
}

func NewNotifier(discordURL, callbackURL, callbackSecret string) *Notifier {
	return &Notifier{
		Discord:  NewDiscordClient(discordURL),
		Callback: NewCallbackClient(callbackURL, callbackSecret),
	}
}

func (n *Notifier) NotifyNewWebService(discordInfo WebServiceInfo, callbackPayload NewWebServicePayload) {
	n.Discord.SendNewWebService(discordInfo)
	n.Callback.SendNewWebService(callbackPayload)
}

func (n *Notifier) NotifyDigest(kind string, info DigestInfo) {
	n.Discord.SendDigest(info)
	n.Callback.SendDigest(kind, info)
}

// FormatTechnologies extracts "Name Version" strings from raw JSON technologies.
func FormatTechnologies(raw json.RawMessage) ([]string, []TechInfo) {
	if len(raw) == 0 {
		return nil, nil
	}

	var techs []TechInfo
	if err := json.Unmarshal(raw, &techs); err != nil {
		return nil, nil
	}

	names := make([]string, 0, len(techs))
	for _, t := range techs {
		s := t.Name
		if t.Version != "" {
			s += " " + t.Version
		}
		names = append(names, s)
	}
	return names, techs
}

// ExtractFavicon extracts favicon URL from raw JSON metadata.
func ExtractFavicon(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var meta struct {
		Favicon string `json:"favicon"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return ""
	}
	return meta.Favicon
}

// ExtractChainInfo extracts status_code and title from the first chain entry.
func ExtractChainInfo(raw json.RawMessage) (int, string) {
	if len(raw) == 0 {
		return 0, ""
	}
	var chain []struct {
		StatusCode int    `json:"status_code"`
		Title      string `json:"title"`
	}
	if err := json.Unmarshal(raw, &chain); err != nil || len(chain) == 0 {
		return 0, ""
	}
	return chain[0].StatusCode, chain[0].Title
}

// BuildWebServiceNotification builds Discord and Callback notification payloads
// from a fingerprint result, used by the fingerprint queue worker.
func BuildWebServiceNotification(
	url, wildcardValue, hostnameID string,
	chain, technologies, metadata json.RawMessage,
) (WebServiceInfo, NewWebServicePayload) {
	statusCode, title := ExtractChainInfo(chain)
	techNames, techInfos := FormatTechnologies(technologies)
	favicon := ExtractFavicon(metadata)

	discordInfo := WebServiceInfo{
		URL:          url,
		Wildcard:     wildcardValue,
		StatusCode:   statusCode,
		Title:        title,
		Technologies: techNames,
	}

	callbackPayload := NewWebServicePayload{
		URL:        url,
		Wildcard:   wildcardValue,
		HostnameID: hostnameID,
		Fingerprint: FingerprintSummary{
			StatusCode:   statusCode,
			Title:        title,
			Technologies: techInfos,
			Favicon:      favicon,
		},
		DiscoveredAt: time.Now().UTC().Format(time.RFC3339),
	}

	return discordInfo, callbackPayload
}
