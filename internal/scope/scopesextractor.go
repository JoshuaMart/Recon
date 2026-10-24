package scope

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Entry is one scope of ScopesExtractor's GET /.
type Entry struct {
	Slug        string `json:"slug"`
	Platform    string `json:"platform"`
	ProgramName string `json:"program_name"`
	Bounty      bool   `json:"bounty"`
	Value       string `json:"value"`
	Type        string `json:"type"`
	IsInScope   bool   `json:"is_in_scope"`
}

// Source is where the sync reads scopes from.
type Source interface {
	Scopes(ctx context.Context) ([]Entry, error)
}

// Client reads ScopesExtractor's REST API.
type Client struct {
	base string
	key  string
	http *http.Client
}

// NewClient builds a client for the instance at base, authenticated with key.
func NewClient(base, key string, timeout time.Duration) *Client {
	return &Client{base: strings.TrimSuffix(base, "/"), key: key, http: &http.Client{Timeout: timeout}}
}

// maxBody bounds the answer: a few thousand programs fit in a few megabytes.
const maxBody = 64 << 20

// Scopes returns every in-scope entry ScopesExtractor holds.
func (c *Client) Scopes(ctx context.Context) ([]Entry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-KEY", c.key)
	req.Header.Set("Accept", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("scopesextractor: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return nil, fmt.Errorf("scopesextractor: %s: %s", res.Status, strings.TrimSpace(string(snippet)))
	}
	var body struct {
		Scopes []Entry `json:"scopes"`
		Count  *int    `json:"count"`
	}
	dec := json.NewDecoder(io.LimitReader(res.Body, maxBody))
	if err := dec.Decode(&body); err != nil {
		return nil, fmt.Errorf("scopesextractor: decode: %w", err)
	}
	// A count that disagrees with the list is a truncated or malformed answer,
	// and reconciling against it would close rules that still exist.
	if body.Count == nil || *body.Count != len(body.Scopes) {
		return nil, fmt.Errorf("scopesextractor: count %v does not match %d scopes", body.Count, len(body.Scopes))
	}
	return body.Scopes, nil
}
