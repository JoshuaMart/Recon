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

// maxRefusedEntries bounds the identities an answer names one by one. The decoder
// bounds its own refusals and the contract says the list is bounded, so this half
// holds up its end: ten thousand single-label hostnames are ten thousand identical
// refusals.
const maxRefusedEntries = 100

// Imported is what one file became. Counts and per-scope totals, not a row per
// asset: a ten-thousand-entry list is not an answer anyone reads. Refusals stay
// individual, since entries silently going nowhere is this endpoint's failure
// mode and a total hides exactly that.
type Imported struct {
	Scan   bbot.Provenance            `json:"scan"`
	Events map[string]*bbot.TypeCount `json:"events"`
	Assets Tally                      `json:"assets"`
	// TypesBeyond is how many event types the decoder stopped naming: never reached
	// by a real file, immediately by a malformed one.
	TypesBeyond int `json:"types_beyond,omitempty"`
	// Refused is the entries the file named that could not become an identity, plus
	// lines that were not events. Bounded, with a final entry counting the rest.
	Refused []Refused `json:"refused"`
	// beyond is what the bound held back, reported in that final entry.
	beyond int
}

// Tally is what the write did.
type Tally struct {
	Created  int `json:"created"`
	Existing int `json:"existing"`
	Hosts    int `json:"hosts"`
	Services int `json:"services"`
	// ByScope is the perimeter's verdict on what arrived, the number to read first: a
	// file whose assets are all unknown was imported into the wrong programme, and
	// nothing else says so.
	ByScope map[string]int `json:"by_scope"`
	// Scheduled is how many will actually be looked at. Everything outside the
	// perimeter is stored and never probed, and an answer not separating the two shows
	// an inventory nothing will check.
	Scheduled int `json:"scheduled"`
}

// Import writes a scan somebody ran elsewhere. It declares assets and writes no observation:
// an observation claims a state held between two dates as measured by an instrument this
// platform versions, which an imported file is not. It uses the same statement as everything
// else, one round trip per asset: a batched insert would be a second implementation of
// classification, scheduling and revival, agreeing with the first until one is fixed. The
// bound in front of the endpoint makes that affordable.
func (i *Ingestor) Import(
	ctx context.Context, q *sqlcgen.Queries, run Run, set *scope.Set, scan bbot.Scan,
) (Imported, error) {
	run.Source = SourceBBOT
	// An import does not revive: the file's date is the scanner's and can predate the
	// archival it would undo.
	run.Revive = false

	out := Imported{
		Scan:    scan.Provenance,
		Events:  scan.Counts,
		Assets:  Tally{ByScope: map[string]int{}},
		Refused: []Refused{},
	}
	for _, refused := range scan.Refused {
		out.refuse("line "+strconv.Itoa(refused.Line), refused.Reason)
	}
	out.beyond += scan.RefusedBeyond
	out.TypesBeyond = scan.TypesBeyond

	now := i.now()
	// Hosts first: a service needs its host's identifier as parent, and the file is
	// walked in the order this decoder fixed, not the events' arrival order.
	hosts := make(map[string]uuid.UUID, len(scan.Hosts))
	for _, host := range scan.Hosts {
		key, err := hostKey(host.Name)
		if err != nil {
			out.refuse(host.Name, err.Error())
			continue
		}

		// The ordinary stagger, not the immediate date a certificate gets. One name
		// published seconds ago earns a check now; thousands of names of unknown age in
		// one transaction are the convoy the jitter breaks up.
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
		// A service whose host was refused is refused with it. The decoder creates a host
		// for every service it emits, so a missing name is one this platform could not
		// make an identity. normalize disagrees with hostKey on one shape, the
		// single-label host (Service accepts it, FQDN does not); writing the service
		// anyway would leave one with no parent and no scheduling.
		parent, ok := hosts[service.Host]
		if !ok {
			out.refuse(service.Host+":"+strconv.Itoa(service.Port), "its host was not an identity")
			continue
		}

		key, err := normalize.Service(service.Host, service.Port, "tcp")
		if err != nil {
			out.refuse(service.Host+":"+strconv.Itoa(service.Port), err.Error())
			continue
		}

		// No due date of its own: a service is observed through its host's run, and one
		// with a date would sit in a queue nothing dispatches from.
		accepted, err := i.enterAsset(ctx, q, run, set, key, placement{
			parent: &parent, lineage: serviceLineage(service),
		})
		if err != nil {
			return out, err
		}
		out.tally(accepted, false)
	}

	out.close()
	return out, nil
}

// refuse names one entry up to the bound and counts the rest.
func (out *Imported) refuse(entry, reason string) {
	if len(out.Refused) >= maxRefusedEntries {
		out.beyond++
		return
	}
	out.Refused = append(out.Refused, Refused{Entry: entry, Reason: reason})
}

// close says what the bound held back: a list stopping at a hundred silently reads
// as a file with a hundred bad entries.
func (out *Imported) close() {
	if out.beyond == 0 {
		return
	}
	out.Refused = append(out.Refused, Refused{
		Entry:  strconv.Itoa(out.beyond) + " further entries",
		Reason: "not listed individually, the list stops at " + strconv.Itoa(maxRefusedEntries),
	})
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
	// BBOT's own verdict, recorded and never acted on: it derives from the seed that
	// tool was given, a different perimeter by a different person.
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
