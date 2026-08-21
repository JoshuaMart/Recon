package ingest

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/JoshuaMart/recon/internal/normalize"
)

// maxFaviconBytes is what a favicon may weigh to be kept.
//
// The value is chosen by the target, and nothing stops a server serving five
// megabytes under that name. Past the bound the image is simply not stored: the
// hash and its counter keep working and only the thumbnail is missing, which is
// honest degradation rather than a surprise on a storage bill.
//
// It matches the constraint on the column, and that is the load-bearing part
// rather than tidiness. Without this check the oversized image reaches the
// database, the constraint refuses it, and the refusal takes the whole
// ingestion transaction with it: one target serving a large icon would stop the
// render of every asset in that batch from being written at all.
const maxFaviconBytes = 64 << 10

// pivots is what the projection lifts out of a payload.
//
// A facet aggregates over what the table holds, and a counter maintained on
// write cannot be maintained if the write never sees the value. So this is the
// precondition for the whole of the search chapter rather than a convenience.
type pivots struct {
	FaviconHash   *string
	ScriptHashes  []string
	CookieNames   []string
	ExternalHosts []string
	// TechRender is [{name, version}], encoded. Objects rather than names,
	// because the render is the only producer that knows a version; the column
	// keeps the names, and the object is the evidence.
	TechRender []byte
	// StatusChain is one code per hop, and the render's alone. Showing 200 on a
	// service is true without being the information: the probe may have
	// obtained a 308, then a 307, then a 200, and landed somewhere else
	// entirely. The scanner reports the redirect URLs and the final code and
	// never the code of each hop, so the browser is the only observer that
	// holds this.
	StatusChain []int32
	// CertSPKIHash comes from the probe and never from the render, and the
	// reason is coverage rather than tidiness. A browser would get it for free
	// having already completed the handshake, but the probe sees every HTTPS
	// service on every full pass while a render happens on five triggers that
	// can be three weeks apart, or never on an asset with no baseline. A pivot
	// present on a fraction of the inventory joins nothing.
	CertSPKIHash *string
}

// favicon is the image itself, which is not a pivot but the depiction of one.
type favicon struct {
	Hash      string
	MediaType string
	Bytes     []byte
}

// liftPivots reads the pivots out of a normalized payload.
//
// From the normalized payload rather than from the producer's document, and
// that is the load-bearing part: the journal stores the normalized form, so
// reading anything else here would let the projection and the journal disagree
// about what was seen. Cookie names are the clearest case, since normalization
// is what drops the ones generated per session.
func liftPivots(layer normalize.Layer, data map[string]any) pivots {
	switch layer {
	case normalize.LayerFingerprint:
		return pivots{
			StatusChain:   hopStatuses(data["chain"]),
			FaviconHash:   text(faviconHash(data)),
			ScriptHashes:  internalScriptHashes(data["scripts"]),
			CookieNames:   textList(data["cookie_names"]),
			ExternalHosts: textList(data["external_hosts"]),
			TechRender:    renderedTechnologies(data["technologies"]),
		}

	case normalize.LayerHTTP:
		tls, ok := data["tls"].(map[string]any)
		if !ok {
			return pivots{}
		}
		hash, _ := tls["cert_spki_hash"].(string)
		return pivots{CertSPKIHash: text(hash)}

	default:
		return pivots{}
	}
}

// hopStatuses reads one code per hop, in order.
//
// The sequence is the information, so it is never sorted: a reordered chain
// claims the request took another route. A single hop is still written, and the
// console is what decides that an arrow pointing at nothing is noise.
func hopStatuses(value any) []int32 {
	hops, ok := value.([]any)
	if !ok || len(hops) == 0 {
		return nil
	}
	codes := make([]int32, 0, len(hops))
	for _, entry := range hops {
		hop, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		// Through float64 because the payload has been through JSON, where
		// every number is one.
		code, ok := hop["status_code"].(float64)
		if !ok {
			continue
		}
		codes = append(codes, int32(code))
	}
	if len(codes) == 0 {
		return nil
	}
	return codes
}

func faviconHash(data map[string]any) string {
	metadata, ok := data["metadata"].(map[string]any)
	if !ok {
		return ""
	}
	hash, _ := metadata["favicon_hash"].(string)
	return hash
}

// internalScriptHashes keeps the scripts the target actually serves.
//
// The internal flag is not a reading detail. A bundle served from a public CDN
// is shared by thousands of unrelated sites, so it groups without
// discriminating, which is the test that decides what is a pivot at all.
func internalScriptHashes(value any) []string {
	scripts, ok := value.([]any)
	if !ok {
		return nil
	}
	var hashes []string
	for _, entry := range scripts {
		script, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		internal, _ := script["internal"].(bool)
		hash, _ := script["hash"].(string)
		if internal && hash != "" {
			hashes = append(hashes, hash)
		}
	}
	return unique(hashes)
}

// renderedTechnologies keeps the name and the version, and drops the rest.
//
// The proof and the category stay in the journal, where the asset view reads
// them. What goes into the projection is what a row shows and what the column
// is derived from, and carrying the evidence into the hottest write path would
// pay for it on every asset to display it on one.
func renderedTechnologies(value any) []byte {
	found, ok := value.([]any)
	if !ok {
		return nil
	}

	type technology struct {
		Name    string `json:"name"`
		Version string `json:"version,omitempty"`
	}
	seen := map[string]bool{}
	kept := make([]technology, 0, len(found))
	for _, entry := range found {
		tech, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		name, _ := tech["name"].(string)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		version, _ := tech["version"].(string)
		kept = append(kept, technology{Name: name, Version: version})
	}
	if len(kept) == 0 {
		return nil
	}
	// Sorted, so that two passes reporting the same set in a different order
	// write the same object. Unsorted, the projection would differ on every
	// pass for no reason a person could see.
	sort.Slice(kept, func(a, b int) bool { return kept[a].Name < kept[b].Name })

	encoded, err := json.Marshal(kept)
	if err != nil {
		return nil
	}
	return encoded
}

// liftFavicon decodes the inline image, and says why it did not.
//
// The service sends it as a data URI and sends null past its own inline bound,
// which is a different thing from an absent icon: the first says the icon was
// too large, the second that there is none, and a consumer confusing them
// concludes that a 90 KB icon does not exist.
func liftFavicon(data map[string]any) (favicon, bool) {
	hash := faviconHash(data)
	if hash == "" {
		return favicon{}, false
	}
	metadata, ok := data["metadata"].(map[string]any)
	if !ok {
		return favicon{}, false
	}
	uri, _ := metadata["favicon"].(string)
	if uri == "" {
		return favicon{}, false
	}

	mediaType, payload, ok := splitDataURI(uri)
	if !ok {
		return favicon{}, false
	}
	bytes, err := base64.StdEncoding.DecodeString(payload)
	if err != nil || len(bytes) == 0 || len(bytes) > maxFaviconBytes {
		return favicon{}, false
	}
	return favicon{Hash: hash, MediaType: mediaType, Bytes: bytes}, true
}

// splitDataURI reads "data:image/png;base64,...." and refuses anything else.
//
// Only base64, because that is what the service sends and because a percent
// encoded variant would need a second decoder for a case nothing produces.
func splitDataURI(uri string) (mediaType, payload string, ok bool) {
	rest, found := strings.CutPrefix(uri, "data:")
	if !found {
		return "", "", false
	}
	header, payload, found := strings.Cut(rest, ",")
	if !found {
		return "", "", false
	}
	mediaType, encoding, found := strings.Cut(header, ";")
	if !found || encoding != "base64" || mediaType == "" {
		return "", "", false
	}
	return mediaType, payload, true
}

// textList reads a payload array of strings.
func textList(value any) []string {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok && text != "" {
			out = append(out, text)
		}
	}
	return unique(out)
}

// unique sorts and deduplicates, so a repetition is one value.
//
// A page loading the same bundle twice is one asset carrying that hash, not
// two, and the counter answers "how many assets share this value".
func unique(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	sort.Strings(values)
	out := values[:1]
	for _, value := range values[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}
	return out
}

// describe is what a log line says about a favicon that was dropped.
func (f favicon) describe() string { return fmt.Sprintf("%s (%d bytes)", f.MediaType, len(f.Bytes)) }
