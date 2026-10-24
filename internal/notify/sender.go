package notify

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"time"

	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

// backoff is the wait after each failed attempt; past its end the
// notification is given up.
var backoff = []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, 15 * time.Minute, 15 * time.Minute}

// Sender delivers the notifications that are due.
type Sender struct {
	Q        *sqlcgen.Queries
	Channels []Channel
	Log      *slog.Logger
	Now      func() time.Time
	Tick     time.Duration
}

func (s *Sender) now() time.Time { return s.Now().UTC().Truncate(time.Microsecond) }

// Loop sends until ctx ends.
func (s *Sender) Loop(ctx context.Context) {
	t := time.NewTicker(s.Tick)
	defer t.Stop()
	for {
		if err := s.Pass(ctx); err != nil && ctx.Err() == nil {
			s.Log.ErrorContext(ctx, "notification pass", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Pass sends what is due. A failed message waits its backoff; one that failed
// on every attempt is kept with its last error.
func (s *Sender) Pass(ctx context.Context) error {
	due, err := s.Q.DueNotifications(ctx, s.now())
	if err != nil {
		return err
	}
	for _, n := range due {
		if len(s.Channels) == 0 {
			if err := s.Q.NotificationSent(ctx, sqlcgen.NotificationSentParams{ID: n.ID, Outcome: ptr("no_channel"), Now: s.now()}); err != nil {
				return err
			}
			continue
		}
		m := Compose(n)
		var errs []error
		for _, c := range s.Channels {
			// A retry goes to the channels that did not get it.
			if slices.Contains(n.Delivered, c.Name()) {
				continue
			}
			if err := c.Send(ctx, m); err != nil {
				errs = append(errs, errors.New(c.Name()+": "+err.Error()))
				continue
			}
			if err := s.Q.NotificationDelivered(ctx, sqlcgen.NotificationDeliveredParams{ID: n.ID, Channel: c.Name()}); err != nil {
				return err
			}
		}
		if len(errs) == 0 {
			if err := s.Q.NotificationSent(ctx, sqlcgen.NotificationSentParams{ID: n.ID, Outcome: ptr("sent"), Now: s.now()}); err != nil {
				return err
			}
			continue
		}
		msg := errors.Join(errs...).Error()
		s.Log.WarnContext(ctx, "notification not delivered", "id", n.ID, "attempt", n.Attempts+1, "err", msg)
		if int(n.Attempts) >= len(backoff) {
			err = s.Q.NotificationSent(ctx, sqlcgen.NotificationSentParams{ID: n.ID, Outcome: ptr("failed"), Now: s.now(), Error: &msg})
		} else {
			err = s.Q.NotificationRetry(ctx, sqlcgen.NotificationRetryParams{ID: n.ID, Due: s.now().Add(backoff[n.Attempts]), Error: &msg})
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func ptr[T any](v T) *T { return &v }
