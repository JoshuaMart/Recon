// Package inventory writes assets, their deduplicated observations and the
// changes between them. Every write path to those tables goes through here.
package inventory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/JoshuaMart/recon/internal/lifecycle"
	"github.com/JoshuaMart/recon/internal/scope"
	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

// Asset kinds.
const (
	KindFQDN    = "fqdn"
	KindIP      = "ip"
	KindService = "service"
	KindURL     = "url"
)

// Asset is what an upsert describes.
type Asset struct {
	ProgramID uuid.UUID
	Kind      string
	Key       string
	Host      string
	Port      int // 0 when the kind has none
	Scheme    string
	Parent    *uuid.UUID
	Wildcard  *uuid.UUID
	Source    string
	// Path is the lineage of the first appearance. It is kept as written.
	Path []map[string]any
	// Lifecycle is the state a new asset starts in.
	Lifecycle string
	// Sighting moves last_seen: true for what a scanner saw, false for a
	// declaration, which is not evidence that anything answers.
	Sighting bool
}

// Written is the result of an upsert.
type Written struct {
	ID    uuid.UUID
	New   bool
	Scope string
}

// Writer writes inventory rows for one program, inside one transaction.
type Writer struct {
	Q          *sqlcgen.Queries
	ProgramID  uuid.UUID
	Classifier *scope.Classifier
	RunID      *uuid.UUID
	Now        time.Time
	// ProgramActive gates due dates: a paused or archived program has none.
	ProgramActive bool
	Cadences      *lifecycle.Cadences // nil: the defaults
	Rand          func() float64      // nil: math/rand
}

// Upsert creates or sights an asset, classifies it against the rules in force,
// and records the creation or a scope move as a change.
func (w *Writer) Upsert(ctx context.Context, a Asset) (Written, error) {
	status := w.Classifier.Classify(scope.Target{Kind: a.Kind, Key: a.Key, Host: a.Host, Port: a.Port, Scheme: a.Scheme})
	path, err := json.Marshal(nonNil(a.Path))
	if err != nil {
		return Written{}, err
	}
	row, err := w.Q.UpsertAsset(ctx, sqlcgen.UpsertAssetParams{
		ProgramID: w.ProgramID, Kind: a.Kind, Key: a.Key, Host: a.Host, Port: optInt(a.Port), Scheme: optStr(a.Scheme),
		ParentAssetID: a.Parent, WildcardID: a.Wildcard, DiscoverySource: a.Source, DiscoveryPath: path,
		ScopeStatus: status, Now: w.Now, Sighting: a.Sighting,
	})
	if err != nil {
		return Written{}, fmt.Errorf("asset %s %s: %w", a.Kind, a.Key, err)
	}
	isNew := row.OldScopeStatus == ""
	if isNew {
		if err := w.Q.InsertAssetCurrent(ctx, sqlcgen.InsertAssetCurrentParams{
			AssetID: row.ID, ProgramID: w.ProgramID, Kind: a.Kind, Key: a.Key, Host: a.Host,
			Port: optInt(a.Port), Scheme: optStr(a.Scheme), ScopeStatus: status, Lifecycle: a.Lifecycle, Now: w.Now,
		}); err != nil {
			return Written{}, err
		}
		if err := w.change(ctx, row.ID, "new_asset", "", nil, map[string]any{"kind": a.Kind, "key": a.Key, "source": a.Source}); err != nil {
			return Written{}, err
		}
		return Written{ID: row.ID, New: true, Scope: status}, nil
	}
	if a.Sighting || row.OldScopeStatus != status {
		if err := w.Q.TouchAssetCurrent(ctx, sqlcgen.TouchAssetCurrentParams{AssetID: row.ID, ScopeStatus: status, Now: w.Now}); err != nil {
			return Written{}, err
		}
	}
	if row.OldScopeStatus != status {
		if err := w.change(ctx, row.ID, "scope_changed", "", map[string]any{"scope": row.OldScopeStatus}, map[string]any{"scope": status}); err != nil {
			return Written{}, err
		}
	}
	return Written{ID: row.ID, Scope: status}, nil
}

// SetLifecycle moves an asset to state and records the move. It does nothing
// when the asset is already there.
func (w *Writer) SetLifecycle(ctx context.Context, id uuid.UUID, state string) error {
	cur, err := w.Q.GetAssetCurrent(ctx, id)
	if err != nil {
		return err
	}
	if cur.Lifecycle == state {
		return nil
	}
	if err := w.Q.SetLifecycle(ctx, sqlcgen.SetLifecycleParams{AssetID: id, Lifecycle: state, Now: w.Now}); err != nil {
		return err
	}
	return w.change(ctx, id, "lifecycle_changed", "", map[string]any{"lifecycle": cur.Lifecycle}, map[string]any{"lifecycle": state})
}

// Observed is what writing an observation did.
type Observed struct {
	// Inserted is false when the observation confirmed the previous one.
	Inserted bool
	// Before is the previous observation's data, nil for the first one.
	Before json.RawMessage
}

// Observe writes one observation, deduplicated against the last one of the
// same asset and layer: identical outcome and data only extend the previous
// row. data must already be normalized. The producer version is stored apart
// and never compared, so an upgrade alone writes nothing.
func (w *Writer) Observe(ctx context.Context, assetID uuid.UUID, layer, outcome string, data any, version string) (Observed, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return Observed{}, err
	}
	last, err := w.Q.LastObservation(ctx, sqlcgen.LastObservationParams{AssetID: assetID, Layer: layer, Outcome: outcome, Data: raw})
	switch {
	case err == nil && last.Same:
		return Observed{}, w.Q.ConfirmObservation(ctx, sqlcgen.ConfirmObservationParams{
			ID: last.ID, ObservedAt: last.ObservedAt, Now: w.Now, Version: optStr(version),
		})
	case err != nil && !errors.Is(err, pgx.ErrNoRows):
		return Observed{}, err
	}
	if err := w.Q.InsertObservation(ctx, sqlcgen.InsertObservationParams{
		AssetID: assetID, Layer: layer, Outcome: outcome, Data: raw, Now: w.Now, RunID: w.RunID, Version: optStr(version),
	}); err != nil {
		return Observed{}, err
	}
	res := Observed{Inserted: true}
	if err == nil {
		res.Before = last.Data
		if err := w.Q.MarkChanged(ctx, sqlcgen.MarkChangedParams{AssetID: assetID, Now: w.Now}); err != nil {
			return Observed{}, err
		}
	}
	return res, nil
}

// Change records one change on an asset.
func (w *Writer) Change(ctx context.Context, assetID uuid.UUID, kind, layer string, before, after any) error {
	return w.change(ctx, assetID, kind, layer, before, after)
}

func (w *Writer) change(ctx context.Context, assetID uuid.UUID, kind, layer string, before, after any) error {
	b, err := marshalOpt(before)
	if err != nil {
		return err
	}
	a, err := marshalOpt(after)
	if err != nil {
		return err
	}
	return w.Q.InsertChange(ctx, sqlcgen.InsertChangeParams{
		AssetID: assetID, ProgramID: w.ProgramID, Kind: kind, Layer: optStr(layer), Before: b, After: a, RunID: w.RunID, Now: w.Now,
	})
}

func marshalOpt(v any) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	if r, ok := v.(json.RawMessage); ok {
		return r, nil
	}
	return json.Marshal(v)
}

// HostAsset describes a host by name or address: an address literal is an ip
// asset, anything else an fqdn. name must be canonical.
func HostAsset(name string) (kind, key string) {
	if a, err := netip.ParseAddr(name); err == nil {
		return KindIP, a.Unmap().String()
	}
	return KindFQDN, name
}

// ServiceKey is host:port/tcp, with an IPv6 host in brackets.
func ServiceKey(host string, port int) string {
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return host + ":" + strconv.Itoa(port) + "/tcp"
}

func optInt(v int) *int32 {
	if v == 0 {
		return nil
	}
	n := int32(v) //nolint:gosec // ports are bounded to 65535 by every caller
	return &n
}

func optStr(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

func nonNil(p []map[string]any) []map[string]any {
	if p == nil {
		return []map[string]any{}
	}
	return p
}

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }
