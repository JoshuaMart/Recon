package fingerprint

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type Client struct {
	baseURL    string
	httpClient *http.Client
}

func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

type Result struct {
	URL           string        `json:"url"`
	Chain         json.RawMessage `json:"chain"`
	Technologies  json.RawMessage `json:"technologies"`
	Cookies       json.RawMessage `json:"cookies"`
	Metadata      json.RawMessage `json:"metadata"`
	ExternalHosts json.RawMessage `json:"external_hosts"`
	ScannedAt     time.Time       `json:"scanned_at"`
}

type ErrorResponse struct {
	Error string `json:"error"`
}

type scanRequest struct {
	URL     string      `json:"url"`
	Options scanOptions `json:"options"`
}

type scanOptions struct {
	BrowserDetection bool `json:"browser_detection"`
	TimeoutSeconds   int  `json:"timeout_seconds"`
	MaxRedirects     int  `json:"max_redirects"`
}

func (c *Client) Scan(targetURL string) (*Result, error) {
	body, err := json.Marshal(scanRequest{
		URL: targetURL,
		Options: scanOptions{
			BrowserDetection: true,
			TimeoutSeconds:   30,
			MaxRedirects:     10,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal scan request: %w", err)
	}

	resp, err := c.httpClient.Post(c.baseURL+"/scan", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("fingerprinter request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fingerprinter returned status %d", resp.StatusCode)
	}

	// Try to detect error response
	var raw json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("failed to decode fingerprinter response: %w", err)
	}

	// Check if it's an error
	var errResp ErrorResponse
	if err := json.Unmarshal(raw, &errResp); err == nil && errResp.Error != "" {
		return nil, fmt.Errorf("fingerprinter error: %s", errResp.Error)
	}

	var result Result
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("failed to parse fingerprinter result: %w", err)
	}

	return &result, nil
}
