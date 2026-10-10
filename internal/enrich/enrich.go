// Package enrich derives infrastructure facts from an address. It runs at
// ingestion in the control plane, never in a scanner: the databases weigh tens of
// megabytes, which every cold start would pay. A run reports the address it
// connected to and this turns it into an operator and a place.
package enrich

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strings"

	"github.com/oschwald/maxminddb-golang/v2"
)

// Result is what an address yields. Every field is optional: a database can know
// the operator and not the city, the common case on cloud ranges.
type Result struct {
	ASN     int
	ASNOrg  string
	Country string
	Region  string
	City    string
}

// Empty reports whether the lookup found nothing, which differs from the
// deployment being unable to look.
func (r Result) Empty() bool {
	return r.ASN == 0 && r.ASNOrg == "" && r.Country == "" && r.Region == "" && r.City == ""
}

// Enricher is what ingestion holds. Configured is part of the interface because
// the console cannot deduce it: "not configured" and "configured, no match" both
// give an asset with no operator, and an empty panel for the first sends someone
// looking for a fault.
type Enricher interface {
	Configured() bool
	Lookup(addr netip.Addr) Result
	Close() error
}

// Nothing is the default: a deployment with no database is normal, so this is a
// working implementation and not a nil to check at every call site.
func Nothing() Enricher { return nothing{} }

type nothing struct{}

func (nothing) Configured() bool         { return false }
func (nothing) Lookup(netip.Addr) Result { return Result{} }
func (nothing) Close() error             { return nil }

// Paths names the databases. Either may be empty: the operator is far more
// actionable than the city, and an ASN-only deployment is reasonable.
type Paths struct {
	City string
	ASN  string
}

// Open loads what the paths name. An empty Paths returns Nothing without error; a
// set but unreadable path is an error, as failing silently would leave an
// inventory quietly without operators.
func Open(paths Paths) (Enricher, error) {
	if strings.TrimSpace(paths.City) == "" && strings.TrimSpace(paths.ASN) == "" {
		return Nothing(), nil
	}

	m := &maxmind{}
	var errs []error

	if path := strings.TrimSpace(paths.City); path != "" {
		reader, err := open(path)
		if err != nil {
			errs = append(errs, fmt.Errorf("city database: %w", err))
		}
		m.city = reader
	}
	if path := strings.TrimSpace(paths.ASN); path != "" {
		reader, err := open(path)
		if err != nil {
			errs = append(errs, fmt.Errorf("asn database: %w", err))
		}
		m.asn = reader
	}

	if err := errors.Join(errs...); err != nil {
		_ = m.Close()
		return nil, err
	}
	return m, nil
}

func open(path string) (*maxminddb.Reader, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("%s is not readable: %w", path, err)
	}
	reader, err := maxminddb.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%s is not a MaxMind database: %w", path, err)
	}
	return reader, nil
}

type maxmind struct {
	city *maxminddb.Reader
	asn  *maxminddb.Reader
}

func (m *maxmind) Configured() bool { return m.city != nil || m.asn != nil }

// The subset of the record this model uses. A narrow struct, not a map, keeps a
// database update from silently changing what is stored.
type cityRecord struct {
	Country struct {
		ISOCode string `maxminddb:"iso_code"`
	} `maxminddb:"country"`
	City struct {
		Names map[string]string `maxminddb:"names"`
	} `maxminddb:"city"`
	Subdivisions []struct {
		Names map[string]string `maxminddb:"names"`
	} `maxminddb:"subdivisions"`
}

type asnRecord struct {
	Number int    `maxminddb:"autonomous_system_number"`
	Org    string `maxminddb:"autonomous_system_organization"`
}

func (m *maxmind) Lookup(addr netip.Addr) Result {
	var out Result
	if !addr.IsValid() {
		return out
	}
	// A private-range address has no operator or place; asking wastes a lookup on
	// every scan of an internal target.
	if addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() {
		return out
	}

	if m.asn != nil {
		var record asnRecord
		if result := m.asn.Lookup(addr); result.Found() {
			if err := result.Decode(&record); err == nil {
				out.ASN = record.Number
				out.ASNOrg = record.Org
			}
		}
	}

	if m.city != nil {
		var record cityRecord
		if result := m.city.Lookup(addr); result.Found() {
			if err := result.Decode(&record); err == nil {
				// Deliberately no fallback to registered_country. An anycast address carries only
				// that field, so the fallback would fire on exactly the addresses whose location
				// means least (1.1.1.1 registers as Australia and is served from everywhere). The
				// country exists to spot assets somewhere unusual for their programme, and a
				// registration would invent that signal. An absent country is exact.
				out.Country = record.Country.ISOCode
				out.City = record.City.Names["en"]
				if len(record.Subdivisions) > 0 {
					out.Region = record.Subdivisions[0].Names["en"]
				}
			}
		}
	}

	return out
}

func (m *maxmind) Close() error {
	var errs []error
	if m.city != nil {
		errs = append(errs, m.city.Close())
	}
	if m.asn != nil {
		errs = append(errs, m.asn.Close())
	}
	return errors.Join(errs...)
}
