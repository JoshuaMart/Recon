package ct

import (
	"errors"
	"fmt"

	"github.com/valyala/fastjson"
)

// Decoder turns one lite-stream message into a Frame, reading only the fields Frame
// holds. Profiled in production, encoding/json was a quarter of the control plane's
// CPU: it scans each message twice and decodes by reflection. This parses once and
// is about three times faster on the pinned stream. Not safe for concurrent use.
type Decoder struct {
	parser fastjson.Parser
}

// Decode fills frame from data. A field of the wrong type is an error, as it was
// under encoding/json: a SAN list that silently decoded to nothing would read as a
// certificate with no names.
func (d *Decoder) Decode(data []byte, frame *Frame) error {
	*frame = Frame{}

	root, err := d.parser.ParseBytes(data)
	if err != nil {
		return err
	}
	if frame.MessageType, err = stringAt(root, "message_type"); err != nil {
		return err
	}

	body := root.Get("data")
	if body == nil || body.Type() == fastjson.TypeNull {
		return nil
	}
	if body.Type() != fastjson.TypeObject {
		return errors.New("data: not an object")
	}

	f := &frame.Data
	if f.UpdateType, err = stringAt(body, "update_type"); err != nil {
		return err
	}
	if v := present(body, "cert_index"); v != nil {
		if f.CertIndex, err = v.Int64(); err != nil {
			return fmt.Errorf("cert_index: %w", err)
		}
	}
	if v := present(body, "seen"); v != nil {
		if f.Seen, err = v.Float64(); err != nil {
			return fmt.Errorf("seen: %w", err)
		}
	}

	if v := present(body, "leaf_cert", "all_domains"); v != nil {
		names, err := v.Array()
		if err != nil {
			return fmt.Errorf("all_domains: %w", err)
		}
		f.LeafCert.AllDomains = make([]string, 0, len(names))
		for _, name := range names {
			raw, err := name.StringBytes()
			if err != nil {
				return fmt.Errorf("all_domains: %w", err)
			}
			f.LeafCert.AllDomains = append(f.LeafCert.AllDomains, string(raw))
		}
	}
	if f.LeafCert.Issuer.CN, err = stringAt(body, "leaf_cert", "issuer", "CN"); err != nil {
		return err
	}
	if f.LeafCert.Issuer.O, err = stringAt(body, "leaf_cert", "issuer", "O"); err != nil {
		return err
	}
	if f.Source.Name, err = stringAt(body, "source", "name"); err != nil {
		return err
	}
	if f.Source.URL, err = stringAt(body, "source", "url"); err != nil {
		return err
	}
	return nil
}

// present returns the value at path, or nil when it is absent or null, which
// encoding/json also left at the zero value.
func present(v *fastjson.Value, path ...string) *fastjson.Value {
	found := v.Get(path...)
	if found == nil || found.Type() == fastjson.TypeNull {
		return nil
	}
	return found
}

// stringAt copies the string at path: the parser reuses its buffer, so a slice into
// it would change under the next message.
func stringAt(v *fastjson.Value, path ...string) (string, error) {
	found := present(v, path...)
	if found == nil {
		return "", nil
	}
	raw, err := found.StringBytes()
	if err != nil {
		return "", fmt.Errorf("%s: %w", path[len(path)-1], err)
	}
	return string(raw), nil
}
