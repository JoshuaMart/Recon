package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

type DiscordClient struct {
	webhookURL string
	httpClient *http.Client
}

func NewDiscordClient(webhookURL string) *DiscordClient {
	return &DiscordClient{
		webhookURL: webhookURL,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

type WebServiceInfo struct {
	URL          string
	Wildcard     string
	StatusCode   int
	Title        string
	Technologies []string
}

func (c *DiscordClient) SendNewWebService(info WebServiceInfo) {
	if c.webhookURL == "" {
		return
	}

	fields := []map[string]any{
		{"name": "URL", "value": info.URL, "inline": false},
		{"name": "Wildcard", "value": info.Wildcard, "inline": true},
		{"name": "Status", "value": fmt.Sprintf("%d", info.StatusCode), "inline": true},
	}

	if info.Title != "" {
		fields = append(fields, map[string]any{"name": "Title", "value": info.Title, "inline": true})
	}

	if len(info.Technologies) > 0 {
		fields = append(fields, map[string]any{
			"name":   "Technologies",
			"value":  strings.Join(info.Technologies, ", "),
			"inline": false,
		})
	}

	payload := map[string]any{
		"embeds": []map[string]any{
			{
				"title":     "New web service discovered",
				"color":     5814783,
				"fields":    fields,
				"timestamp": time.Now().UTC().Format(time.RFC3339),
			},
		},
	}

	c.send(payload)
}

type DigestInfo struct {
	Kind               string // "recon" or "revalidation"
	WildcardsProcessed int
	NewHostnames       int
	NewlyDead          int
	NewWebServices     int
	Duration           time.Duration
}

func (c *DiscordClient) SendDigest(info DigestInfo) {
	if c.webhookURL == "" {
		return
	}

	title := "Recon completed"
	if info.Kind == "revalidation" {
		title = "Revalidation completed"
	}

	payload := map[string]any{
		"embeds": []map[string]any{
			{
				"title": title,
				"color": 3066993,
				"fields": []map[string]any{
					{"name": "Wildcards processed", "value": fmt.Sprintf("%d", info.WildcardsProcessed), "inline": true},
					{"name": "New hostnames", "value": fmt.Sprintf("+%d", info.NewHostnames), "inline": true},
					{"name": "Now dead", "value": fmt.Sprintf("-%d", info.NewlyDead), "inline": true},
					{"name": "New web services", "value": fmt.Sprintf("%d", info.NewWebServices), "inline": true},
					{"name": "Duration", "value": formatDuration(info.Duration), "inline": true},
				},
				"timestamp": time.Now().UTC().Format(time.RFC3339),
			},
		},
	}

	c.send(payload)
}

func (c *DiscordClient) send(payload map[string]any) {
	body, err := json.Marshal(payload)
	if err != nil {
		log.Printf("Discord: failed to marshal payload: %v", err)
		return
	}

	resp, err := c.httpClient.Post(c.webhookURL, "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("Discord: failed to send webhook: %v", err)
		return
	}
	_ = resp.Body.Close()

	if resp.StatusCode >= 300 {
		log.Printf("Discord: webhook returned status %d", resp.StatusCode)
	}
}

func formatDuration(d time.Duration) string {
	m := int(d.Minutes())
	s := int(d.Seconds()) % 60
	return fmt.Sprintf("%dm%02ds", m, s)
}
