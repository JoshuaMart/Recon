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

// DefaultTemplate is what a channel gets when it names none.
//
// Discord and Slack are then configuration rather than code: they are webhooks
// with a particular payload shape, and writing a connector in Go would freeze
// what a template expresses and start over at the next one.
const DefaultTemplate = `{"content": {{ .Text | json }}}`

// ErrUnbuildable is a channel that exists and cannot be called.
//
// An organization with no channel and a broken channel are not the same case,
// and confusing them is a way to lose all of an organization's alerts in
// silence. With no channel, that is a deliberate configuration: events are
// marked delivered and counted, so "computed and sent nowhere" stays visible. A
// channel that exists and cannot be built is an outage: resolution fails, the
// events stay queued, and the stuck queue alert does its job.
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

// Sender delivers a rendered message to a channel.
//
// Transport settings belong to the deployment rather than to the channel:
// method, timeout, attempts and backoff apply to every one. The URL, the
// template and the priority floor belong to the row.
type Sender struct {
	http    *http.Client
	secrets map[string]string
}

// NewSender builds one. The secrets are named by a channel and resolved here,
// so no credential is ever stored beside a destination.
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
		// The one function a template needs, and the one it must not be
		// without: a title carrying a quote would otherwise produce a body no
		// endpoint can parse, and the failure would look like the endpoint's.
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

// Send delivers one message.
//
// A channel whose secret_ref resolves to nothing is refused rather than called
// without a credential: an endpoint expecting authentication answers 401, and
// the events would pile up against a target the logs do not let anyone repair.
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

	// notified_at is set only on a 2xx, and that is the one rule stopping a
	// webhook outage from becoming a silent loss of alerts.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("send: %s: %s", resp.Status, bytes.TrimSpace(detail))
	}
	return nil
}
