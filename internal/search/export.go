package search

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Formats an export can be written in.
const (
	// FormatJSONL is the lossless one, a line per asset with attributes and lineage,
	// for feeding another system.
	FormatJSONL = "jsonl"
	// FormatCSV opens in a spreadsheet and flattens; the loss is named here so nobody
	// discovers it looking for a favicon in a column.
	FormatCSV = "csv"
	// FormatURLs feeds web tools: one measured, openable URL per line. Services whose
	// scheme was not observed are omitted, not guessed from their port.
	FormatURLs = "urls"
)

// exportPage is how many rows one round trip carries. The walk reuses the list's
// pagination key, writing as it goes: OFFSET rescans everything before it on
// each page, and materializing first holds the whole inventory in memory.
const exportPage = 500

// Export walks the filtered result and writes it as it goes, with the list's AST,
// organization clause and table (a separate export query would be a second set of rules).
// begin is called once, after the first page returns and before anything is written, and
// hands back the writer: a failure on the first statement can still become an error status,
// so a client does not read a zero-byte file as an empty inventory.
func Export(
	ctx context.Context, q Querier, org uuid.UUID, filter Node, format string, limit int,
	begin func() io.Writer,
) (int, error) {
	switch format {
	case FormatJSONL, FormatCSV, FormatURLs:
	default:
		return 0, refuse("no export format named %q", format)
	}

	var out io.Writer
	var sheet *csv.Writer
	var encoder *json.Encoder

	written := 0
	cursor := Cursor{}
	for {
		page := exportPage
		// A limit can be asked for, never imposed: a silently truncated export is worse
		// than a slow one or a refusal.
		if format != FormatURLs && limit > 0 && limit-written < page {
			page = limit - written
		}
		if page <= 0 {
			break
		}

		// Display off: the list removes a badge, never data, and a display filter here
		// would break that rule invisibly.
		answer, err := List(ctx, q, org, Request{
			Filter: filter, Limit: page, Cursor: cursor, Display: false,
		})
		if err != nil {
			return written, err
		}

		// Nothing has been sent on the first pass, so a failure above is still a status.
		if out == nil {
			out = begin()
			switch format {
			case FormatCSV:
				sheet = csv.NewWriter(out)
				if err := sheet.Write(csvHeader); err != nil {
					return written, fmt.Errorf("write the header: %w", err)
				}
			case FormatJSONL:
				encoder = json.NewEncoder(out)
			}
		}

		for _, row := range answer.Rows {
			if sheet != nil {
				if err := sheet.Write(csvRow(row)); err != nil {
					return written, fmt.Errorf("write a row: %w", err)
				}
			} else if encoder != nil {
				if err := encoder.Encode(row); err != nil {
					return written, fmt.Errorf("write a row: %w", err)
				}
			} else {
				url := exportURL(row)
				if url == "" {
					continue
				}
				if _, err := fmt.Fprintln(out, url); err != nil {
					return written, fmt.Errorf("write a URL: %w", err)
				}
			}
			written++
			// URL exports can skip many rows: keep database pages large, stop once the
			// requested number of lines is emitted.
			if format == FormatURLs && limit > 0 && written == limit {
				return written, nil
			}
		}
		if answer.Next == "" {
			break
		}
		if cursor, err = ParseCursor(answer.Next); err != nil {
			return written, err
		}
	}

	if sheet != nil {
		sheet.Flush()
		if err := sheet.Error(); err != nil {
			return written, fmt.Errorf("flush: %w", err)
		}
	}
	return written, nil
}

// exportURL projects a web asset onto an openable address, with the console's
// identity rule: a declared URL keeps its path, a service is measured scheme plus
// host and port. A port never implies a scheme; IPv6 literals need brackets.
func exportURL(row Row) string {
	switch row.Kind {
	case "url":
		return row.Key
	case "service":
		if row.Scheme == nil || row.Host == nil || row.Port == nil {
			return ""
		}
	default:
		return ""
	}

	if *row.Scheme != "http" && *row.Scheme != "https" {
		return ""
	}

	host := *row.Host
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	port := *row.Port
	if (*row.Scheme != "http" || port != 80) && (*row.Scheme != "https" || port != 443) {
		host += ":" + strconv.Itoa(int(port))
	}
	return *row.Scheme + "://" + host
}

// csvHeader is what flattens: promoted columns, joined technologies, volatility.
// No attributes or lineage, since a nested object in a cell is neither readable
// nor usable.
var csvHeader = []string{
	"asset_id", "kind", "key", "host", "port", "scheme", "lifecycle", "scope_status",
	"status_code", "final_url", "title", "server", "ip", "asn", "asn_org", "country",
	"city", "is_cdn", "cdn_provider", "waf_detected", "waf_vendor", "technologies",
	"volatility", "discovery_source", "first_seen", "last_seen", "last_changed_at",
}

func csvRow(row Row) []string {
	return []string{
		row.AssetID.String(),
		row.Kind,
		row.Key,
		text(row.Host),
		number(row.Port),
		text(row.Scheme),
		row.Lifecycle,
		row.ScopeStatus,
		number(row.StatusCode),
		text(row.FinalURL),
		text(row.Title),
		text(row.Server),
		address(row),
		number(row.ASN),
		text(row.ASNOrg),
		text(row.Country),
		text(row.City),
		flag(row.IsCDN),
		text(row.CDNProvider),
		flag(row.WAFDetected),
		text(row.WAFVendor),
		strings.Join(row.Tech, " "),
		strconv.Itoa(row.Volatility),
		row.Source,
		row.FirstSeen.UTC().Format("2006-01-02T15:04:05Z"),
		row.LastSeen.UTC().Format("2006-01-02T15:04:05Z"),
		instant(row.LastChangedAt),
	}
}

func text(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func number(value *int32) string {
	if value == nil {
		return ""
	}
	return strconv.Itoa(int(*value))
}

func flag(value *bool) string {
	if value == nil {
		return ""
	}
	return strconv.FormatBool(*value)
}

func address(row Row) string {
	if row.IP == nil {
		return ""
	}
	return row.IP.String()
}

func instant(value *time.Time) string {
	if value == nil {
		return ""
	}
	return value.UTC().Format("2006-01-02T15:04:05Z")
}
