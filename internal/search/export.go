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
	// FormatJSONL is the lossless one, one line per asset, attributes and
	// lineage included. It is the one for feeding another system.
	FormatJSONL = "jsonl"
	// FormatCSV is the one that opens in a spreadsheet, and it flattens. The
	// loss is named here rather than discovered by somebody looking for a
	// favicon in a column.
	FormatCSV = "csv"
)

// exportPage is how many rows one round trip carries.
//
// The walk reuses the list's pagination key page by page, writing as it goes.
// An OFFSET on a million rows costs a scan of everything before it on every
// page, and materializing the result before sending it holds the whole inventory
// in memory for a file nobody reads until it is finished.
const exportPage = 500

// Export walks the filtered result and writes it as it goes.
//
// The same AST, the same organization clause and the same table as the list.
// There is no export query, which would be a second set of rules to keep in
// sync with the first.
func Export(
	ctx context.Context, q Querier, org uuid.UUID, filter Node, format string, limit int, w io.Writer,
) (int, error) {
	switch format {
	case FormatJSONL, FormatCSV:
	default:
		return 0, refuse("no export format named %q", format)
	}

	rows := w
	var sheet *csv.Writer
	if format == FormatCSV {
		sheet = csv.NewWriter(w)
		if err := sheet.Write(csvHeader); err != nil {
			return 0, fmt.Errorf("write the header: %w", err)
		}
		rows = io.Discard
	}
	encoder := json.NewEncoder(rows)

	written := 0
	cursor := Cursor{}
	for {
		page := exportPage
		// A limit can be asked for; it is never imposed. A truncated export
		// that says nothing is the worst of the three possible behaviours,
		// ahead of the slow export and ahead of the refusal.
		if limit > 0 && limit-written < page {
			page = limit - written
		}
		if page <= 0 {
			break
		}

		// Display off. The list removes a badge and never data, and an export
		// applying a display filter would do exactly what that rule forbids
		// while making it invisible.
		answer, err := List(ctx, q, org, Request{
			Filter: filter, Limit: page, Cursor: cursor, Display: false,
		})
		if err != nil {
			return written, err
		}
		for _, row := range answer.Rows {
			if format == FormatCSV {
				if err := sheet.Write(csvRow(row)); err != nil {
					return written, fmt.Errorf("write a row: %w", err)
				}
			} else if err := encoder.Encode(row); err != nil {
				return written, fmt.Errorf("write a row: %w", err)
			}
			written++
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

// csvHeader is what flattens.
//
// Promoted columns, joined technologies, volatility. No attributes and no
// lineage, because a nested object in a cell is neither readable nor usable, and
// naming the loss here is better than leaving somebody to find it.
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
