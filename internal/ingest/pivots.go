package ingest

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/JoshuaMart/recon/internal/normalize"
)

// maxFaviconBytes is what a favicon may weigh to be kept. The target chooses the value and
// could serve five megabytes; past the bound the image is not stored (hash and counter keep
// working, only the thumbnail is missing). It matches the column constraint on purpose: else
// the database would refuse the image and the refusal would take the whole ingestion
// transaction, one large icon blocking every asset in the batch.
const maxFaviconBytes = 64 << 10

// pivots is what the projection lifts out of a payload. A counter maintained on
// write cannot be maintained if the write never sees the value, so this is a
// precondition of search, not a convenience.
type pivots struct {
	FaviconHash   *string
	ScriptHashes  []string
	CookieNames   []string
	ExternalHosts []string
	// TechRender is [{name, version}], encoded. Objects, because only the render knows
	// a version; the column keeps names and the object is the evidence.
	TechRender []byte
	// StatusChain is one code per hop, the render's alone. A 200 on a service is true
	// but not the information: the probe may have seen 308, 307, then 200 and landed
	// elsewhere. The scanner reports redirect URLs and the final code, never each
	// hop's, so the browser is the only observer holding this.
	StatusChain []int32
	// CertSPKIHash comes from the probe, never the render, for coverage: a browser
	// gets it free after the handshake, but the probe sees every HTTPS service on
	// every full pass while renders happen on five triggers weeks apart, or never. A
	// pivot present on a fraction of the inventory joins nothing.
	CertSPKIHash *string
}

// favicon is the image itself: not a pivot but the depiction of one.
type favicon struct {
	Hash      string
	MediaType string
	Bytes     []byte
}

// liftPivots reads the pivots from a normalized payload, not the producer's
// document: the journal stores the normalized form, and reading anything else
// could make projection and journal disagree about what was seen. Cookie names
// are the clearest case, as normalization drops the per-session ones.
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

// hopStatuses reads one code per hop, in order. The sequence is the information
// and is never sorted: a reordered chain claims another route. A single hop is
// still written; the console decides an arrow to nothing is noise.
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
		// Through float64: the payload has been through JSON.
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

// internalScriptHashes keeps the scripts the target actually serves. The internal
// flag matters: a bundle from a public CDN is shared by thousands of unrelated
// sites, so it groups without discriminating, the test for what is a pivot.
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

// renderedTechnologies keeps name and version. Proof and category stay in the
// journal for the asset view; the projection holds what a row shows, since
// carrying evidence into the hottest write path pays on every asset to display it
// on one.
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
	// Sorted, so two passes reporting the same set in another order write the same
	// object; unsorted, the projection would differ every pass for no visible reason.
	sort.Slice(kept, func(a, b int) bool { return kept[a].Name < kept[b].Name })

	encoded, err := json.Marshal(kept)
	if err != nil {
		return nil
	}
	return encoded
}

// liftFavicon decodes the inline image and says why it did not. The service sends
// a data URI, and null past its own inline bound; that differs from an absent
// icon (too large versus none), and confusing them concludes a 90 KB icon does
// not exist.
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

// splitDataURI reads "data:image/png;base64,..." and refuses anything else. Only
// base64: it is what the service sends, and a percent-encoded variant would need
// a second decoder for a case nothing produces.
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

// unique sorts and deduplicates. A page loading one bundle twice is one asset
// carrying that hash, and the counter answers "how many assets share this value".
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
