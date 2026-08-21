// The tenant guard declares, it does not deduce.
//
// A guard that looks for org_id in the text of a query cannot decide isolation:
// a correlated subquery carrying a.org_id = b.org_id satisfies it while
// returning every tenant. So the burden is inverted. Every query says what it
// is, this checks that it said something, that what it said is not contradicted
// by something simple and exact, and that a crossing carries a reason.
//
// The list of cross-org queries it produces is the point. It is the exact
// specification of what asm_sys has to cover, and it does not exist while
// undetected cross-tenant queries pass in silence.
package store_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// declarations a query may carry.
const (
	// scoped names org_id itself, as a filter or as a value it supplies.
	scoped = "scoped"
	// keyed is confined to one identifier, and isolated by the policy behind
	// it. It is the form most statements take, and calling it scoped would put
	// it in the column that says "this one carries its own filter".
	keyed = "keyed"
	// crossOrg crosses tenants on purpose. It is the column that specifies the
	// system role, so it is the one that has to stay short and reasoned.
	crossOrg = "cross-org"
	// none touches no table carrying org_id.
	none = "none"
)

var known = map[string]bool{scoped: true, keyed: true, crossOrg: true, none: true}

var (
	nameLine    = regexp.MustCompile(`^-- name: (\w+) :(\w+)$`)
	tenantLine  = regexp.MustCompile(`^-- @tenant: (\S+)$`)
	whyLine     = regexp.MustCompile(`^-- @why: (.*)$`)
	createTable = regexp.MustCompile(`^CREATE TABLE (?:IF NOT EXISTS )?(\w+) \($`)
)

// query is one statement and what it declared about itself.
type query struct {
	name    string
	file    string
	line    int
	tenant  string
	why     string
	body    string
	tenants []string // the tenant tables it names
}

func TestEveryQueryDeclaresItsTenancy(t *testing.T) {
	tables := tenantTables(t)
	queries := readQueries(t, tables)

	if len(queries) == 0 {
		t.Fatal("no queries were read, which means the parser is broken rather than the queries clean")
	}

	for _, q := range queries {
		where := q.file + ":" + itoa(q.line)

		if q.tenant == "" {
			t.Errorf("%s: %s carries no -- @tenant: declaration", where, q.name)
			continue
		}
		if !known[q.tenant] {
			t.Errorf("%s: %s declares %q, which is not one of scoped, keyed, cross-org, none",
				where, q.name, q.tenant)
			continue
		}

		// The three checks that are simple and exact. Anything cleverer would
		// be the deduction this guard exists to refuse.
		switch q.tenant {
		case crossOrg:
			if strings.TrimSpace(q.why) == "" {
				t.Errorf("%s: %s crosses tenants and gives no reason. A crossing without one is the "+
					"list that specifies asm_sys growing by an entry nobody argued for", where, q.name)
			}
		case scoped:
			if !strings.Contains(q.body, "org_id") {
				t.Errorf("%s: %s declares scoped and never names org_id", where, q.name)
			}
		case none:
			if len(q.tenants) > 0 {
				t.Errorf("%s: %s declares none and reads %s, which carries org_id",
					where, q.name, strings.Join(q.tenants, ", "))
			}
		case keyed:
			if len(q.tenants) == 0 {
				t.Errorf("%s: %s declares keyed and touches no tenant table, so it is none",
					where, q.name)
			}
		}
	}
}

// TestTheCrossingsAreTheOnesArguedFor pins the inventory.
//
// Not because the list must never grow, but because it must never grow by
// accident. A query that starts crossing tenants is a deployment decision: it
// has to move onto the system pool, and that is not something a reviewer
// notices in a diff of SQL.
func TestTheCrossingsAreTheOnesArguedFor(t *testing.T) {
	expected := []string{
		"CountUnobservable",
		"ExpireRuns",
		"OnboardingDue",
		"OneOrganization",
		"OrgForRun",
		"PendingEvents",
		"PrincipalForToken",
		"ProgramsDueForDiscovery",
		"PurgeSuppressed",
		"SelectDueRenders",
		"StuckEvents",
		"ThirdPartyHosts",
	}

	var found []string
	for _, q := range readQueries(t, tenantTables(t)) {
		if q.tenant == crossOrg {
			found = append(found, q.name)
		}
	}
	sort.Strings(found)

	if strings.Join(found, ",") != strings.Join(expected, ",") {
		t.Errorf("the cross-tenant inventory changed\n  have: %v\n  want: %v\n"+
			"Every entry here has to run on the system pool. Update deployment 9.6 and this list together.",
			found, expected)
	}
}

// tenantTables reads the tables carrying org_id out of the migrations.
//
// From the catalog rather than from a list kept by hand, because a list kept by
// hand is exactly what forgets the table added last week.
func tenantTables(t *testing.T) map[string]bool {
	t.Helper()

	// The tenant itself carries id rather than org_id, and every policy on it
	// reads that column. It is a tenant table by definition.
	tables := map[string]bool{"org": true}

	paths, err := filepath.Glob(filepath.Join("..", "..", "db", "migrations", "*.sql"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("read the migrations: %v", err)
	}

	for _, path := range paths {
		raw, err := os.ReadFile(path) //nolint:gosec // a path this test built itself
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		lines := strings.Split(string(raw), "\n")
		for i := 0; i < len(lines); i++ {
			m := createTable.FindStringSubmatch(strings.TrimSpace(lines[i]))
			if m == nil {
				continue
			}
			for j := i + 1; j < len(lines); j++ {
				line := lines[j]
				// The body ends at the first line starting a closing paren,
				// which covers both ");" and ") PARTITION BY ...".
				if strings.HasPrefix(strings.TrimSpace(line), ")") {
					i = j
					break
				}
				if strings.Contains(line, "org_id") {
					tables[m[1]] = true
				}
			}
		}
	}
	return tables
}

// readQueries splits the query files into statements and their preamble.
func readQueries(t *testing.T, tables map[string]bool) []query {
	t.Helper()

	paths, err := filepath.Glob(filepath.Join("..", "..", "db", "queries", "*.sql"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("read the queries: %v", err)
	}
	sort.Strings(paths)

	var out []query
	for _, path := range paths {
		raw, err := os.ReadFile(path) //nolint:gosec // a path this test built itself
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		out = append(out, parse(filepath.Base(path), string(raw), tables)...)
	}
	return out
}

func parse(file, content string, tables map[string]bool) []query {
	lines := strings.Split(content, "\n")

	// Where each statement is named. Found first, because a query's preamble
	// and the previous query's body are the same lines read from two ends.
	var names []int
	for i, line := range lines {
		if nameLine.MatchString(strings.TrimSpace(line)) {
			names = append(names, i)
		}
	}

	var out []query
	for k, at := range names {
		m := nameLine.FindStringSubmatch(strings.TrimSpace(lines[at]))
		q := query{name: m[1], file: file, line: at + 1}

		// The preamble is the unbroken run of comment and blank lines just
		// above the name. Reading further would let a declaration belonging to
		// the previous statement answer for this one.
		start := at
		for start > 0 {
			prev := strings.TrimSpace(lines[start-1])
			if prev != "" && !strings.HasPrefix(prev, "--") {
				break
			}
			start--
		}
		for _, line := range lines[start:at] {
			line = strings.TrimSpace(line)
			if d := tenantLine.FindStringSubmatch(line); d != nil {
				q.tenant = d[1]
			}
			if w := whyLine.FindStringSubmatch(line); w != nil {
				q.why = w[1]
			}
		}

		// The body runs to the next statement's preamble, or to the end of the
		// file for the last one.
		end := len(lines)
		if k+1 < len(names) {
			end = names[k+1]
		}
		q.body = stripComments(strings.Join(lines[at+1:end], "\n"))
		q.tenants = mentions(q.body, tables)
		out = append(out, q)
	}
	return out
}

// stripComments removes the prose so a table named in a comment is not read as
// a table the statement touches.
func stripComments(body string) string {
	var kept []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		if idx := strings.Index(line, "--"); idx >= 0 {
			line = line[:idx]
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// mentions finds the tenant tables a statement names.
//
// Word boundaries rather than substrings: "org" appears inside "org_id" and
// inside "program", and either would make every statement in the repository
// touch the tenant table.
func mentions(body string, tables map[string]bool) []string {
	var found []string
	for table := range tables {
		re := regexp.MustCompile(`(^|[^\w.])` + regexp.QuoteMeta(table) + `([^\w]|$)`)
		if re.MatchString(body) {
			found = append(found, table)
		}
	}
	sort.Strings(found)
	return found
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
