package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/JoshuaMart/recon/internal/normalize"
	"github.com/JoshuaMart/recon/internal/scope"
	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

// Entered is what a hand entered list produced.
type Entered struct {
	Accepted []Accepted `json:"accepted"`
	Refused  []Refused  `json:"refused"`
}

// Accepted is one entry that became inventory.
type Accepted struct {
	AssetID uuid.UUID `json:"asset_id"`
	Kind    string    `json:"kind"`
	Key     string    `json:"key"`
	Scope   string    `json:"scope_status"`
	Created bool      `json:"created"`
	// Scheduled says whether anything will actually go and look. An entry
	// outside the perimeter is stored and never probed, and a console that did
	// not say so would show an asset nothing is ever going to check.
	Scheduled bool `json:"scheduled"`
}

// Refused is one entry that was not an identity.
type Refused struct {
	Entry  string `json:"entry"`
	Reason string `json:"reason"`
}

// Sources an entered asset can carry, and they are two because the lineage
// answers "why is this here" and the two answers are different acts.
//
// One is somebody typing a name into the assets form. The other is a perimeter
// rule naming a host or a path, which declares the thing as well as classifying
// it: an apex says where to enumerate, and those two say what exists.
const (
	SourceManual = "manual"
	SourceRule   = "scope_rule"
)

// origin is the source this entry records and the step its lineage carries.
//
// Defaulted rather than required, so a caller that says nothing keeps the
// behaviour the assets form has always had.
func (r Run) origin() (string, string) {
	if r.Source == SourceRule {
		return SourceRule, "declared_in_scope"
	}
	return SourceManual, "entered_by_hand"
}

// Enter records assets somebody typed in.
//
// It runs under the scope action rather than the ingestion one. Entering an
// asset by hand is an assertion about the perimeter, which is a different
// privilege from writing what a scanner found: a run that could do this could
// widen its own mandate.
//
// A host is due for full, not for resolve. Somebody typed it in to find out
// what it exposes, and a resolution would only report that the name answers.
// The ladder makes that free to say, because full runs every rung below it, so
// one run gives the resolution, the open ports and the services behind them.
// It is the only case where the first run of an asset is the expensive one, and
// it is the right place for it, because a person is waiting.
func (i *Ingestor) Enter(
	ctx context.Context, q *sqlcgen.Queries, run Run, set *scope.Set, entries []string,
) (Entered, error) {
	at := i.now()
	run.Due = Schedule{Resolve: &at, Full: &at}

	out := Entered{Accepted: []Accepted{}, Refused: []Refused{}}
	for _, entry := range entries {
		accepted, err := i.enter(ctx, q, run, set, entry, at)
		if err != nil {
			var bad *refusal
			if errors.As(err, &bad) {
				out.Refused = append(out.Refused, Refused{Entry: entry, Reason: bad.reason})
				continue
			}
			return out, err
		}
		out.Accepted = append(out.Accepted, accepted...)
	}
	return out, nil
}

func (i *Ingestor) enter(
	ctx context.Context, q *sqlcgen.Queries, run Run, set *scope.Set, entry string, at time.Time,
) ([]Accepted, error) {
	if looksLikeURL(entry) {
		return i.enterURL(ctx, q, run, set, entry)
	}

	key, err := hostKey(entry)
	if err != nil {
		return nil, &refusal{reason: err.Error()}
	}
	accepted, err := i.enterAsset(ctx, q, run, set, key, nil, run.Due)
	if err != nil {
		return nil, err
	}
	return []Accepted{accepted}, nil
}

// enterURL records a path somebody named.
//
// This is the one case where a path is an identity rather than the place a
// redirect landed. Adding one creates or finds the service it belongs to and
// schedules that service, because a URL has no liveness of its own: what
// answers is the service. The URL earns its render once the service has
// answered, through the ordinary baseline filter, and the renderer is given the
// URL as declared rather than the service root.
func (i *Ingestor) enterURL(
	ctx context.Context, q *sqlcgen.Queries, run Run, set *scope.Set, entry string,
) ([]Accepted, error) {
	url, err := normalize.URL(entry)
	if err != nil {
		return nil, &refusal{reason: err.Error()}
	}

	host, err := hostKey(url.Host)
	if err != nil {
		return nil, &refusal{reason: err.Error()}
	}
	service, err := normalize.Service(url.Host, url.Port, "tcp")
	if err != nil {
		return nil, &refusal{reason: err.Error()}
	}

	// The host is what a run targets, so it is the one that carries the due
	// date. Scheduling the service means scheduling the host it sits on.
	hostAsset, err := i.enterAsset(ctx, q, run, set, host, nil, run.Due)
	if err != nil {
		return nil, err
	}
	hostID := hostAsset.AssetID

	serviceAsset, err := i.enterAsset(ctx, q, run, set, service, &hostID, Schedule{})
	if err != nil {
		return nil, err
	}
	serviceID := serviceAsset.AssetID

	urlAsset, err := i.enterAsset(ctx, q, run, set, url, &serviceID, Schedule{})
	if err != nil {
		return nil, err
	}

	return []Accepted{hostAsset, serviceAsset, urlAsset}, nil
}

func (i *Ingestor) enterAsset(
	ctx context.Context, q *sqlcgen.Queries, run Run, set *scope.Set,
	key normalize.Key, parent *uuid.UUID, due Schedule,
) (Accepted, error) {
	status := set.Classify(scope.Target{Key: key})

	source, step := run.origin()
	path, err := json.Marshal([]any{map[string]any{
		"step": step,
		"run":  run.ID.String(),
	}})
	if err != nil {
		return Accepted{}, err
	}

	params := sqlcgen.UpsertAssetAndProjectionParams{
		AssetID:         uuidTo(uuid.New()),
		OrgID:           uuidTo(run.OrgID),
		ProgramID:       uuidTo(run.ProgramID),
		Kind:            string(key.Kind),
		Key:             key.Value,
		Host:            text(key.Host),
		DiscoverySource: source,
		DiscoveryPath:   path,
		ScopeStatus:     string(status),
		SeenAt:          stamp(i.now()),
		// An act rather than an observation, and the one documented way an
		// archived asset comes back by hand.
		Revive: true,
	}
	if key.Port != 0 {
		params.Port = portPtr(key.Port)
	}
	if key.Scheme != "" {
		params.Scheme = text(key.Scheme)
	}
	if parent != nil {
		params.ParentAssetID = uuidTo(*parent)
	}
	if status == scope.InScope {
		params.NextResolveAt = stampPtr(due.Resolve)
		params.NextFullAt = stampPtr(due.Full)
	}

	row, err := q.UpsertAssetAndProjection(ctx, params)
	if err != nil {
		return Accepted{}, fmt.Errorf("enter %s: %w", key.Value, err)
	}

	return Accepted{
		AssetID:   uuid.UUID(row.AssetID.Bytes),
		Kind:      string(key.Kind),
		Key:       key.Value,
		Scope:     string(status),
		Created:   row.Created,
		Scheduled: status == scope.InScope && due.Full != nil,
	}, nil
}

// refusal is an entry that was never an identity, as opposed to a write that
// failed. One is the caller's problem and the other is the system's, and
// answering the same way for both makes a typo look like an outage.
type refusal struct{ reason string }

func (r *refusal) Error() string { return r.reason }

// looksLikeURL separates a declared path from a name.
//
// The scheme is the whole test. A bare host is a host whatever it contains, and
// a string carrying a scheme is somebody naming a surface rather than a
// machine.
func looksLikeURL(entry string) bool {
	lower := strings.ToLower(strings.TrimSpace(entry))
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")
}
