package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"text/template"
	"time"

	"github.com/google/uuid"
)

// DefaultTemplate is what a channel gets when it names none. Discord and Slack are
// configuration, not code: webhooks with a payload shape, and a Go connector would
// freeze what a template expresses and restart at the next.
const DefaultTemplate = `{"content": {{ .Text | json }}}`

// ErrUnbuildable is a channel that exists and cannot be called. No channel and a
// broken channel differ, and confusing them loses an organization's alerts
// silently. No channel is deliberate: events are marked delivered and counted, so
// "computed and sent nowhere" stays visible. A channel that cannot be built is an
// outage: resolution fails, events stay queued and the stuck queue alert fires.
var ErrUnbuildable = errors.New("the channel cannot be built")

// Channel is one destination.
type Channel struct {
	ID          uuid.UUID
	URL         string
	SecretRef   string
	Template    string
	MinPriority string
	ManagedBy   string
}

// Message is what a template renders from.
type Message struct {
	Kind      string
	Priority  string
	Program   string
	Asset     string
	Text      string
	Summary   string
	CreatedAt time.Time
	Payload   map[string]any
}

// Sender delivers a rendered message to a channel. Transport settings (method,
// timeout, attempts, backoff) belong to the deployment; URL, template and priority
// floor belong to the row.
type Sender struct {
	http    *http.Client
	secrets map[string]string
}

// NewSender builds one. A channel names its secrets and they are resolved here, so
// no credential is stored beside a destination.
func NewSender(timeout time.Duration, secrets map[string]string) *Sender {
	if secrets == nil {
		secrets = map[string]string{}
	}
	return &Sender{http: &http.Client{Timeout: timeout}, secrets: secrets}
}

// Render turns a message into the body a channel expects.
func (s *Sender) Render(channel Channel, message Message) ([]byte, error) {
	text := channel.Template
	if strings.TrimSpace(text) == "" {
		text = DefaultTemplate
	}

	parsed, err := template.New("payload").Funcs(template.FuncMap{
		// The one function a template needs: a title with a quote would otherwise produce
		// a body no endpoint can parse, and the failure would look like the endpoint's.
		"json": func(value any) (string, error) {
			encoded, err := json.Marshal(value)
			return string(encoded), err
		},
	}).Parse(text)
	if err != nil {
		return nil, fmt.Errorf("%w: template: %w", ErrUnbuildable, err)
	}

	var body bytes.Buffer
	if err := parsed.Execute(&body, message); err != nil {
		return nil, fmt.Errorf("%w: render: %w", ErrUnbuildable, err)
	}
	return body.Bytes(), nil
}

// Send delivers one message. A channel whose secret_ref resolves to nothing is
// refused, not called without a credential: the endpoint would answer 401 and
// events would pile up against a target the logs do not let anyone repair.
func (s *Sender) Send(ctx context.Context, channel Channel, message Message) error {
	body, err := s.Render(channel, message)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, channel.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnbuildable, err)
	}
	req.Header.Set("Content-Type", "application/json")

	if channel.SecretRef != "" {
		secret, known := s.secrets[channel.SecretRef]
		if !known || secret == "" {
			return fmt.Errorf("%w: the secret %q resolves to nothing", ErrUnbuildable, channel.SecretRef)
		}
		req.Header.Set("Authorization", secret)
	}

	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("send: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// notified_at is set only on a 2xx, the one rule stopping a webhook outage from
	// becoming silent alert loss.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("send: %s: %s", resp.Status, bytes.TrimSpace(detail))
	}
	return nil
}
