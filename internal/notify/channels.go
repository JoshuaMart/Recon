package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// A Channel delivers a message somewhere.
type Channel interface {
	Name() string
	Send(ctx context.Context, m Message) error
}

var client = &http.Client{Timeout: 15 * time.Second}

func post(ctx context.Context, target string, body any, header func(http.Header)) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(b))
	if err != nil {
		return errors.New("bad channel URL")
	}
	req.Header.Set("Content-Type", "application/json")
	if header != nil {
		header(req.Header)
	}
	res, err := client.Do(req) //nolint:gosec // a URL from the config, not from a request
	if err != nil {
		return stripURL(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(res.Body, 200))
		return fmt.Errorf("answered %d: %s", res.StatusCode, strings.TrimSpace(string(snippet)))
	}
	return nil
}

// stripURL drops the URL net/http puts in its errors: a webhook URL holds its
// token, and the error is logged and stored.
func stripURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s: %w", ue.Op, ue.Err)
	}
	return err
}

// discordLimit is the most characters a Discord message holds.
const discordLimit = 2000

const fence = "```"

// Discord posts to a Discord webhook.
type Discord struct{ URL string }

// Name names the channel.
func (Discord) Name() string { return "discord" }

// Send posts the message in a code block, so nothing read on a target renders
// as markdown or a link, with Recon's own link after it. Mentions are
// disabled: a title read on a target must not ping anyone.
func (d Discord) Send(ctx context.Context, m Message) error {
	var b strings.Builder
	b.WriteString(m.Title)
	for _, it := range m.Items {
		b.WriteString("\n  " + it)
	}
	if m.More > 0 {
		fmt.Fprintf(&b, "\n  and %d more", m.More)
	}
	// A fence inside would close the block early.
	body := strings.ReplaceAll(b.String(), fence, "'''")
	room := discordLimit - 2*len(fence) - 2 - len(m.Link) - 1
	if r := []rune(body); len(r) > room {
		body = string(r[:room-1]) + "…"
	}
	content := fence + "\n" + body + "\n" + fence
	if m.Link != "" {
		content += "\n" + m.Link
	}
	return post(ctx, d.URL, map[string]any{
		"content":          content,
		"allowed_mentions": map[string]any{"parse": []string{}},
	}, nil)
}

// Webhook posts the message as JSON to any URL.
type Webhook struct {
	URL string
	// Header is "Name: value", sent with each call when set.
	Header string
}

// Name names the channel.
func (Webhook) Name() string { return "webhook" }

// Send posts the message's fields.
func (w Webhook) Send(ctx context.Context, m Message) error {
	return post(ctx, w.URL, m, func(h http.Header) {
		if name, value, ok := strings.Cut(w.Header, ":"); ok {
			h.Set(strings.TrimSpace(name), strings.TrimSpace(value))
		}
	})
}
