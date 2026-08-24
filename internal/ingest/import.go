package ingest

import (
	"context"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/JoshuaMart/recon/internal/bbot"
	"github.com/JoshuaMart/recon/internal/normalize"
	"github.com/JoshuaMart/recon/internal/scope"
	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

// Imported is what one file became.
//
// Counts and per scope totals rather than a row per asset, because an import is
// the case the assets form's bound points at and a list of ten thousand entries
// is not an answer anybody reads. Refusals stay individual: entries that
// silently went nowhere are the failure mode this endpoint has, and a total
// hides exactly that.
type Imported struct {
	Scan   bbot.Provenance            `json:"scan"`
	Events map[string]*bbot.TypeCount `json:"events"`
	Assets Tally                      `json:"assets"`
	// TypesBeyond is how many event types the decoder stopped naming. A real
	// file never reaches that bound and a malformed one reaches it at once.
	TypesBeyond int `json:"types_beyond,omitempty"`
	// Refused is the entries the file named and this platform could not turn
	// into an identity, plus the lines that were not events at all.
	Refused []Refused `json:"refused"`
}

// Tally is what the write did.
type Tally struct {
	Created  int `json:"created"`
	Existing int `json:"existing"`
	Hosts    int `json:"hosts"`
	Services int `json:"services"`
	// ByScope is the perimeter's verdict on what arrived, and it is the number
	// worth reading first. A file whose assets are all unknown was imported
	// into the wrong programme, and nothing else in this answer says so.
	ByScope map[string]int `json:"by_scope"`
	// Scheduled is how many will actually be looked at. Everything outside the
	// perimeter is stored and never probed, and an answer that did not separate
	// the two would show an inventory nothing is going to check.
	Scheduled int `json:"scheduled"`
}

// Import writes a scan somebody ran elsewhere.
//
// It declares assets and writes no observation, which 7.6 argues at length and
// this comment will not repeat. The short version is that an observation claims
// a state held between two dates as measured by an instrument this platform
// versions, and an imported file satisfies neither half.
//
// The write is the same statement everything else goes through, one round trip
// per asset. A batched insert would be faster and it would also be a second
// implementation of classification, scheduling and revival, which agree with
// the first until somebody fixes one of them. The bound in front of the
// endpoint is what makes that affordable, and a measured import that takes too
// long is what should change it.
func (i *Ingestor) Import(
	ctx context.Context, q *sqlcgen.Queries, run Run, set *scope.Set, scan bbot.Scan,
) (Imported, error) {
	run.Source = SourceBBOT
	// An import does not revive. The file's date is the scanner's, not this
	// platform's, and it can be older than the archival it would undo.
	run.Revive = false

	out := Imported{
		Scan:    scan.Provenance,
		Events:  scan.Counts,
		Assets:  Tally{ByScope: map[string]int{}},
		Refused: []Refused{},
	}
	for _, refused := range scan.Refused {
		out.Refused = append(out.Refused, Refused{
			Entry:  "line " + strconv.Itoa(refused.Line),
			Reason: refused.Reason,
		})
	}
	// What the decoder's own bounds held back. A list that stops at a hundred
	// and says nothing reads as a file with a hundred bad lines.
	if scan.RefusedBeyond > 0 {
		out.Refused = append(out.Refused, Refused{
			Entry:  strconv.Itoa(scan.RefusedBeyond) + " further lines",
			Reason: "not listed individually, the refusal list stops at " + strconv.Itoa(len(scan.Refused)),
		})
	}
	out.TypesBeyond = scan.TypesBeyond

	now := i.now()
	// Hosts first, because a service needs its host's identifier to record a
	// parent, and the file is walked in an order this decoder fixed rather than
	// the one the events arrived in.
	hosts := make(map[string]uuid.UUID, len(scan.Hosts))
	for _, host := range scan.Hosts {
		key, err := hostKey(host.Name)
		if err != nil {
			out.Refused = append(out.Refused, Refused{Entry: host.Name, Reason: err.Error()})
			continue
		}

		// The ordinary stagger, and not the immediate date a certificate gets.
		// One name published seconds ago earns being checked now; thousands of
		// names of unknown age arriving in one transaction are the convoy the
		// jitter exists to break up.
		due := now.Add(i.cadence.Stagger(i.random()))
		accepted, err := i.enterAsset(ctx, q, run, set, key, placement{
			due:     Schedule{Resolve: &due},
			lineage: hostLineage(host),
		})
		if err != nil {
			return out, err
		}
		hosts[host.Name] = accepted.AssetID
		out.tally(accepted, true)
	}

	for _, service := range scan.Services {
		// A service whose host was refused is refused with it. The decoder
		// creates a host for every service it emits, so a name missing from the
		// map above is one this platform could not turn into an identity, and
		// normalize disagrees with hostKey on exactly one shape: a single label
		// host, which Service accepts and FQDN does not. Writing the service
		// anyway would create one with no parent, which is a service whose
		// scheduling nothing carries.
		parent, ok := hosts[service.Host]
		if !ok {
			out.Refused = append(out.Refused, Refused{
				Entry:  service.Host + ":" + strconv.Itoa(service.Port),
				Reason: "its host was not an identity",
			})
			continue
		}

		key, err := normalize.Service(service.Host, service.Port, "tcp")
		if err != nil {
			out.Refused = append(out.Refused, Refused{
				Entry:  service.Host + ":" + strconv.Itoa(service.Port),
				Reason: err.Error(),
			})
			continue
		}

		// No due date of its own. A service is observed through its host's run,
		// and one given a date would sit in a queue nothing dispatches from.
		accepted, err := i.enterAsset(ctx, q, run, set, key, placement{
			parent: &parent, lineage: serviceLineage(service),
		})
		if err != nil {
			return out, err
		}
		out.tally(accepted, false)
	}

	return out, nil
}

// tally accumulates one written asset.
func (out *Imported) tally(accepted Accepted, isHost bool) {
	if accepted.Created {
		out.Assets.Created++
	} else {
		out.Assets.Existing++
	}
	if isHost {
		out.Assets.Hosts++
	} else {
		out.Assets.Services++
	}
	out.Assets.ByScope[accepted.Scope]++
	if accepted.Scheduled {
		out.Assets.Scheduled++
	}
}

// hostLineage is what the file said about why this name is here.
func hostLineage(host bbot.Host) map[string]any {
	lineage := map[string]any{"tool": "bbot"}
	putString(lineage, "module", host.Module)
	putString(lineage, "context", host.Context)
	// BBOT's own verdict, recorded and never acted on. It is computed from the
	// seed that tool was given, which is a different perimeter described by a
	// different person, and this platform classifies for itself.
	putString(lineage, "tool_scope", host.Scope)
	if !host.At.IsZero() {
		lineage["at"] = host.At.UTC().Format(time.RFC3339)
	}
	if len(host.Technologies) > 0 {
		lineage["technologies"] = host.Technologies
	}
	return lineage
}

// serviceLineage is the same for a port, plus what fingerprintx concluded.
func serviceLineage(service bbot.Service) map[string]any {
	lineage := map[string]any{"tool": "bbot"}
	putString(lineage, "module", service.Module)
	putString(lineage, "context", service.Context)
	putString(lineage, "tool_scope", service.Scope)
	putString(lineage, "protocol", service.Protocol)
	putString(lineage, "banner", service.Banner)
	if !service.At.IsZero() {
		lineage["at"] = service.At.UTC().Format(time.RFC3339)
	}
	return lineage
}

func putString(into map[string]any, name, value string) {
	if value != "" {
		into[name] = value
	}
}
