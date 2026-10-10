package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/JoshuaMart/recon/internal/lifecycle"
	"github.com/JoshuaMart/recon/internal/normalize"
	"github.com/JoshuaMart/recon/internal/scope"
	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

// Entered is what a hand-entered list produced.
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
	// Scheduled says whether anything will look at it. An entry outside the
	// perimeter is stored and never probed, and the console must say so.
	Scheduled bool `json:"scheduled"`
}

// Refused is one entry that was not an identity.
type Refused struct {
	Entry  string `json:"entry"`
	Reason string `json:"reason"`
}

// Sources an entered asset can carry. Lineage answers "why is this here", and
// typing a name in the assets form differs from a perimeter rule naming a host
// or a path, which declares the thing as well as classifying it.
const (
	SourceManual     = "manual"
	SourceRule       = "scope_rule"
	SourceCertstream = "certstream"
	// SourceBBOT is a scan run outside this platform, imported: nobody typed it, no
	// rule declared it, no log published it. Somebody handed over a file.
	SourceBBOT = "bbot"
)

// Certificate is what a candidate's lineage records about the certificate that
// revealed it. "Why is this here" must stay answerable months later, which
// "a certificate" does not and "this issuer, log and index" does; that is why
// the lite stream is dialled rather than the one carrying names alone.
type Certificate struct {
	Issuer string
	Log    string
	Index  int64
}

// origin is the source this entry records and the step its lineage carries.
// Defaulted, so a caller saying nothing keeps the assets form behaviour.
func (r Run) origin() (string, string) {
	switch r.Source {
	case SourceRule:
		return SourceRule, "declared_in_scope"
	case SourceCertstream:
		return SourceCertstream, "certificate_transparency"
	case SourceBBOT:
		return SourceBBOT, "imported"
	default:
		return SourceManual, "entered_by_hand"
	}
}

// Enter records assets somebody typed in. It runs under the scope action, not ingestion:
// entering an asset asserts the perimeter, and a run able to do it could widen its own
// mandate. A host is due for full, not resolve: the person typed it to learn what it
// exposes, and full also runs resolution, ports and services. The one case where an asset's
// first run is the expensive one, because someone is waiting.
func (i *Ingestor) Enter(
	ctx context.Context, q *sqlcgen.Queries, run Run, set *scope.Set, entries []string,
) (Entered, error) {
	at := i.now()
	run.Due = Schedule{Resolve: &at, Full: &at}
	run.Revive = true

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
	accepted, err := i.enterAsset(ctx, q, run, set, key, placement{due: run.Due})
	if err != nil {
		return nil, err
	}
	return []Accepted{accepted}, nil
}

// enterURL records a path somebody named.
//
// The one case where a path is an identity and not where a redirect landed. It
// creates or finds the service it belongs to and schedules that, since a URL has
// no liveness of its own. The URL earns its render once the service has
// answered, and the renderer gets the URL as declared, not the service root.
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

	// The host is what a run targets, so it carries the due date.
	hostAsset, err := i.enterAsset(ctx, q, run, set, host, placement{due: run.Due})
	if err != nil {
		return nil, err
	}
	hostID := hostAsset.AssetID

	serviceAsset, err := i.enterAsset(ctx, q, run, set, service, placement{parent: &hostID})
	if err != nil {
		return nil, err
	}
	serviceID := serviceAsset.AssetID

	urlAsset, err := i.enterAsset(ctx, q, run, set, url, placement{parent: &serviceID})
	if err != nil {
		return nil, err
	}

	return []Accepted{hostAsset, serviceAsset, urlAsset}, nil
}

// placement is what one asset carries beyond its key: where it sits, when it is
// due and why it is here. A struct because two of the three are usually zero
// and a call like enterAsset(..., nil, Schedule{}, nil) hides which mattered.
//
// There is no date: every act writes an asset at the moment it happens, and an
// import's file date belongs in lineage, not on the row.
type placement struct {
	parent *uuid.UUID
	due    Schedule
	// lineage is merged into this source's step, for facts about one asset rather
	// than the batch.
	lineage map[string]any
}

func (i *Ingestor) enterAsset(
	ctx context.Context, q *sqlcgen.Queries, run Run, set *scope.Set,
	key normalize.Key, e placement,
) (Accepted, error) {
	status := set.Classify(scope.Target{Key: key})
	parent, due := e.parent, e.due

	source, step := run.origin()
	lineage := map[string]any{
		"step": step,
		"run":  run.ID.String(),
	}
	if run.Certificate != nil {
		lineage["issuer"] = run.Certificate.Issuer
		lineage["log"] = run.Certificate.Log
		lineage["cert_index"] = run.Certificate.Index
	}
	for name, value := range e.lineage {
		lineage[name] = value
	}
	path, err := json.Marshal([]any{lineage})
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
		// An act, not an observation, and the documented way an archived asset returns
		// by hand. Carried by the run because an import is dated by whoever ran the
		// scan and must not resurrect what this system concluded was gone.
		Revive: run.Revive,
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

	// Scheduled reports what the write did, not what the caller asked: the
	// statement refuses a due date on a row that stays archived.
	archived := row.PreviousLifecycle != nil &&
		*row.PreviousLifecycle == lifecycle.Archived && !run.Revive

	return Accepted{
		AssetID:   uuid.UUID(row.AssetID.Bytes),
		Kind:      string(key.Kind),
		Key:       key.Value,
		Scope:     string(status),
		Created:   row.Created,
		Scheduled: status == scope.InScope && (due.Full != nil || due.Resolve != nil) && !archived,
	}, nil
}

// EnterCandidates records the names one certificate revealed. Same write as the assets form,
// with two differences.
//
// A candidate is due for resolve, not full: most never resolve, so the cheap rung asks
// whether the name exists yet and the expensive one is earned by an answer. And it is
// immediate with no jitter: the certificate is the event and the aggressive curve needs the
// first check now. The clump is normal, a candidate that answers leaves the curve, and the
// convoy forms at the promotion to full, where the jitter is.
//
// An archived name comes back, widening rediscovery on purpose: a fresh certificate for a
// name given up on is the strongest sign someone is provisioning it again, and the budget
// bounds the cost (the revived candidate is archived again by the same rule).
func (i *Ingestor) EnterCandidates(
	ctx context.Context, q *sqlcgen.Queries, run Run, set *scope.Set, names []string,
) (Entered, error) {
	at := i.now()
	run.Source = SourceCertstream
	run.Due = Schedule{Resolve: &at}
	run.Revive = true

	out := Entered{Accepted: []Accepted{}, Refused: []Refused{}}
	for _, name := range names {
		key, err := hostKey(name)
		if err != nil {
			out.Refused = append(out.Refused, Refused{Entry: name, Reason: err.Error()})
			continue
		}

		accepted, err := i.enterAsset(ctx, q, run, set, key, placement{due: run.Due})
		if err != nil {
			return out, err
		}
		out.Accepted = append(out.Accepted, accepted)
	}
	return out, nil
}

// CompileScope reads the rules in force at an instant. The instant is a parameter, not
// now(): valid_to is written by the application, and the database clock would make the
// answer depend on two clocks agreeing. It lives here because it is the query, not the
// predicate, and is exported for three callers (console writes, the assets form, the CT
// matcher): a second copy would be a second answer to what a programme may look at.
func CompileScope(
	ctx context.Context, q *sqlcgen.Queries, programID uuid.UUID, at time.Time,
) (*scope.Set, error) {
	rows, err := q.ListScopeRules(ctx, sqlcgen.ListScopeRulesParams{
		ProgramID: uuidTo(programID),
		At:        stamp(at),
	})
	if err != nil {
		return nil, fmt.Errorf("list scope rules: %w", err)
	}

	rules := make([]scope.Rule, 0, len(rows))
	for _, row := range rows {
		rules = append(rules, scope.Rule{
			ID:      uuid.UUID(row.ID.Bytes).String(),
			Kind:    row.Kind,
			Matcher: row.Matcher,
			Pattern: row.Pattern,
		})
	}
	return scope.Compile(rules)
}

// refusal is an entry that was never an identity, as opposed to a failed write:
// one is the caller's problem, the other the system's.
type refusal struct{ reason string }

func (r *refusal) Error() string { return r.reason }

// looksLikeURL separates a declared path from a name. The scheme is the whole
// test: a bare host is a host whatever it contains.
func looksLikeURL(entry string) bool {
	lower := strings.ToLower(strings.TrimSpace(entry))
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")
}
