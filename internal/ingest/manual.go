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
	SourceManual     = "manual"
	SourceRule       = "scope_rule"
	SourceCertstream = "certstream"
	// SourceBBOT is a scan run outside this platform, imported. It is a fourth
	// act rather than a variant of the first three: nobody typed it, no rule
	// declared it, and no log published it. Somebody handed over a file.
	SourceBBOT = "bbot"
)

// Certificate is what a candidate's lineage records about the certificate that
// revealed it.
//
// It is the reason the lite stream is dialled rather than the cheap one that
// carries names alone: "why is this here" has to be answerable six months later
// by somebody looking at a name they do not recognise, and "a certificate" is
// not an answer where "this issuer, this log, this index" is.
type Certificate struct {
	Issuer string
	Log    string
	Index  int64
}

// origin is the source this entry records and the step its lineage carries.
//
// Defaulted rather than required, so a caller that says nothing keeps the
// behaviour the assets form has always had.
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
// due, and why it is here.
//
// A struct rather than three parameters, because two of the three are usually
// zero and a call site reading enterAsset(..., nil, Schedule{}, nil) says
// nothing about which of them mattered.
//
// There is deliberately no date here. Every act that writes an asset writes it
// at the moment it happens, including an import, whose file carries a date that
// belongs in lineage and not on the row. 7.6 says why.
type placement struct {
	parent *uuid.UUID
	due    Schedule
	// lineage is merged into the step this source records, for the facts that
	// belong to one asset rather than to the whole batch.
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
		// An act rather than an observation, and the one documented way an
		// archived asset comes back by hand. Carried by the run rather than
		// hardcoded here, because an import is dated by whoever ran the scan
		// and must not resurrect what this system concluded was gone.
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

	// Scheduled reports what the write did, not what the caller asked for. The
	// statement refuses a due date on a row that stays archived, so a caller
	// deciding this from its own inputs would answer that something will be
	// looked at when the row it just wrote says otherwise.
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

// EnterCandidates records the names one certificate revealed.
//
// The same act as the assets form and the same write, with two differences that
// both come from who is waiting.
//
// A candidate is due for **resolve** and not for full. Nobody typed this name
// in; a log did, and most candidates never resolve to anything at all. The
// cheap rung answers the only question worth asking first, which is whether the
// name exists yet, and the expensive one is earned by an answer.
//
// It is immediate and takes no jitter, unlike a discovery run's assets. The
// certificate is the event: something was published seconds ago, and the whole
// aggressive curve rests on the first check happening now rather than inside a
// quarter of an hour. The clump that produces is the normal case rather than a
// thing to spread, and a candidate that answers leaves the curve, so the convoy
// jitter exists to prevent never forms here. It forms at the promotion to full,
// which is where the jitter is.
//
// A name that was archived comes back. That is a widening of what 6.2 calls
// rediscovery and it is deliberate: a fresh certificate for a name this system
// gave up on is the strongest signal available that somebody is provisioning it
// again, and it is exactly the case the freshness advantage exists for. The
// budget bounds what it costs, because a revived candidate is chased on the
// same curve and archived again by the same rule.
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

// CompileScope reads the rules in force at an instant.
//
// The instant is a parameter rather than now(): valid_to is written by the
// application and now() is the database's clock, so comparing the two would
// make the answer depend on two clocks agreeing.
//
// It is here rather than in scope because it is the query and not the
// predicate, and it is exported because three callers need it: the console's
// write paths, the assets form, and the Certificate Transparency matcher. A
// second copy would be a second answer to what a programme may look at, and the
// two would agree until somebody fixed one of them.
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
