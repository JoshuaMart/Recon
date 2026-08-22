/**
 * Filters, in the URL and as an AST.
 *
 * The structured representation comes first and the query language later, so
 * there is no text bar here and nothing parses a DSL. What the interface can
 * build is a flat conjunction of predicates, which is exactly what a facet click
 * and a badge click produce, and it travels in the URL as repeated `f` params so
 * that a filtered list is a shareable link and renders on the server.
 *
 * The day the language arrives it produces this same tree. It does not replace
 * this module, it adds a second way to fill it.
 */

export type Op = 'eq' | 'in' | 'suffix' | 'prefix' | 'contains' | 'exists' | 'gt' | 'gte' | 'lt' | 'lte';

export interface Filter {
	field: string;
	op: Op;
	value: string;
}

/**
 * Which fields need a value that is not a string.
 *
 * This is a second registry and the first one lives in Go, which is a divergence
 * waiting to happen. It is small on purpose and it exists for a reason the server
 * cannot solve for us: a facet term is cast to text by the aggregation, so port
 * 443 comes back as "443", and the compiler refuses a string where the column is
 * an int. Everything absent from these two lists travels as text, `program_id`
 * included: the server binds it and casts it at the placeholder.
 */
const numeric = new Set(['port', 'status_code', 'asn', 'volatility']);
const boolean = new Set(['is_cdn', 'waf_detected', 'takeover_candidate']);

/** A node of the filter tree the compiler takes. */
export interface Node {
	op?: string;
	clauses?: Node[];
	field?: string;
	value?: string | number | boolean;
}

/** parse reads the filters out of a URL. An unparseable one is dropped. */
export function parseFilters(params: URLSearchParams): Filter[] {
	const out: Filter[] = [];
	for (const raw of params.getAll('f')) {
		const first = raw.indexOf(':');
		const second = raw.indexOf(':', first + 1);
		if (first < 1 || second < 0) continue;
		const field = raw.slice(0, first);
		const op = raw.slice(first + 1, second) as Op;
		const value = raw.slice(second + 1);
		if (!field || !op || !value) continue;
		out.push({ field, op, value });
	}
	return out;
}

export function encodeFilter(filter: Filter): string {
	return `${filter.field}:${filter.op}:${filter.value}`;
}

/**
 * Every link the interface builds is built here.
 *
 * Not for tidiness: it is the one place a base path would ever have to be
 * applied, and it keeps the components free of URL assembly, which is where an
 * unescaped value slips into a query string.
 */

/** programHref is the list filtered to one perimeter, which is what the programmes
 *  screen links to. `program_id` is a field where `org_id` is not: inside one
 *  organization, asking what a perimeter holds is a legitimate question, and the
 *  tenant clause is emitted beside it either way. */
export function programHref(programID: string): string {
	return href([{ field: 'program_id', op: 'eq', value: programID }]);
}

/**
 * Which shape the list takes, read from the URL.
 *
 * Grouped is the default and the parameter therefore names the exception:
 * `group=none` is the flat list. A name occupies several rows by construction, so
 * the flat list is mostly one host written five times, and a default that has to
 * be chosen every time is a default that is wrong.
 *
 * The two shapes are two routes on the server and not a flag, because their
 * cursors mean different columns. That is why every link below drops the cursor
 * when it changes shape.
 */
export function isGrouped(params: URLSearchParams): boolean {
	return params.get('group') !== 'none';
}

/** href builds the link for a list of filters, dropping the cursor. */
export function href(filters: Filter[], grouped = true): string {
	const query = params(filters);
	if (!grouped) query.set('group', 'none');
	const encoded = query.toString();
	return encoded ? '/?' + encoded : '/';
}

/**
 * groupHref toggles the fold, keeping the filters and dropping the cursor.
 *
 * Dropping it is the point rather than an omission: a cursor of the flat list walks
 * assets and a cursor of the grouped one walks hosts, so carrying one across the
 * toggle would ask the server to continue a walk it never started. The server
 * refuses it, and this makes the refusal unreachable from the interface.
 */
export function groupHref(filters: Filter[], grouped: boolean): string {
	return href(filters, grouped);
}

/** nextHref continues the list. A cursor and never a page number: the server
 *  chose the ordering, and an offset over a million rows scans everything before
 *  it on every page. */
export function nextHref(filters: Filter[], cursor: string, grouped = true): string {
	const search = params(filters);
	if (!grouped) search.set('group', 'none');
	search.set('cursor', cursor);
	return '/?' + search.toString();
}

/** exportHref is the same query as the list, by construction. */
export function exportHref(filters: Filter[], format: 'jsonl' | 'csv' = 'jsonl'): string {
	const search = params(filters);
	search.set('format', format);
	return '/export?' + search.toString();
}

function params(filters: Filter[]): URLSearchParams {
	const search = new URLSearchParams();
	for (const filter of filters) search.append('f', encodeFilter(filter));
	return search;
}

/** withFilter is what a badge or a facet bucket does: add one, drop duplicates. */
export function withFilter(filters: Filter[], filter: Filter): Filter[] {
	const key = encodeFilter(filter);
	if (filters.some((existing) => encodeFilter(existing) === key)) return filters;
	return [...filters, filter];
}

export function withoutFilter(filters: Filter[], filter: Filter): Filter[] {
	const key = encodeFilter(filter);
	return filters.filter((existing) => encodeFilter(existing) !== key);
}

/**
 * toAST compiles the filters into the tree the API expects.
 *
 * No organization clause, and there could not be one: `org_id` is not a field of
 * the server's registry, so the tree cannot express it in either direction. The
 * control plane emits it on every compilation, outside the tree.
 */
export function toAST(filters: Filter[]): Node | undefined {
	if (filters.length === 0) return undefined;

	// Each op goes through as itself, and there is deliberately no "is not". A
	// negated equality drops the rows where the column is NULL, so
	// "cdn_provider is not cloudflare" would exclude every asset that has no CDN
	// at all, which is rarely what somebody means. The interface offers
	// `is_cdn = false` instead, which the facet already carries, and the day a
	// real "or is absent" is needed it is a group with `exists` rather than a
	// clever rewrite here.
	const clauses: Node[] = filters.map((filter) => ({
		field: filter.field,
		op: filter.op,
		value: typed(filter.field, filter.value)
	}));

	if (clauses.length === 1) return clauses[0];
	return { op: 'and', clauses };
}

function typed(field: string, value: string): string | number | boolean {
	if (numeric.has(field)) {
		const parsed = Number(value);
		return Number.isInteger(parsed) ? parsed : value;
	}
	if (boolean.has(field)) {
		return value === 'true';
	}
	return value;
}

/** The chip text. Plain words rather than operators, since there is no DSL yet. */
export function label(filter: Filter, names: Record<string, string> = {}): string {
	const field = fieldLabel(filter.field);
	// An identifier is what the query needs and a name is what a reader needs. The
	// chip shows the name when the caller resolved one, and the identifier when it
	// did not, rather than nothing.
	const value = names[filter.value] ?? filter.value;
	switch (filter.op) {
		case 'suffix':
			return `${field} ends with ${value}`;
		case 'prefix':
			return `${field} starts with ${value}`;
		case 'contains':
			return `${field} includes ${value}`;
		case 'exists':
			return `${field} present`;
		case 'in':
			return `${field} one of ${value}`;
		case 'gt':
			return `${field} over ${value}`;
		case 'gte':
			return `${field} at least ${value}`;
		case 'lt':
			return `${field} under ${value}`;
		case 'lte':
			return `${field} at most ${value}`;
		default:
			return `${field} is ${value}`;
	}
}

const fieldLabels: Record<string, string> = {
	key: 'name',
	program_id: 'program',
	asn_org: 'organization',
	cdn_provider: 'CDN',
	is_cdn: 'fronted',
	status_code: 'status',
	favicon_hash: 'favicon',
	cert_spki_hash: 'certificate key',
	script_hash: 'script',
	cookie_name: 'cookie',
	external_host: 'external host',
	dead_external_host: 'dead external host',
	takeover_candidate: 'takeover candidate'
};

export function fieldLabel(field: string): string {
	return fieldLabels[field] ?? field.replaceAll('_', ' ');
}

/**
 * facetFilter builds the filter a facet bucket links to, with the operator the
 * **server** says the field takes.
 *
 * The sidebar used to write `eq` for every bucket, which is right for a column and
 * wrong for an array: `technologies eq nginx` comes back as
 * `technologies does not accept "eq"`, and only after somebody clicked it. The
 * operators arrive from `GET /assets/fields`, the endpoint whose whole purpose is
 * that a console discovers the vocabulary instead of learning it against 400s.
 *
 * `eq` when the field takes it, `contains` otherwise, and `eq` as a last resort so
 * that a field missing from the map produces a filter the server can name rather
 * than nothing at all.
 */
export function facetFilter(field: string, value: string, operators: Record<string, string[]>): Filter {
	const allowed = operators[field] ?? [];
	if (allowed.includes('eq') || allowed.length === 0) return { field, op: 'eq', value };
	if (allowed.includes('contains')) return { field, op: 'contains', value };
	return { field, op: allowed[0] as Filter['op'], value };
}

/**
 * badgeFilter maps a pivot type back to the field that searches it.
 *
 * The two vocabularies differ, and the mapping belongs here rather than in a
 * component: internal/search names the pivot type `favicon` and the searchable
 * field `favicon_hash`, `cookie_name` against `cookie_name`, and a value keyed on
 * the wrong one silently matches nothing rather than failing.
 */
export function badgeFilter(type: string, value: string): Filter {
	switch (type) {
		case 'favicon':
			return { field: 'favicon_hash', op: 'eq', value };
		case 'cert_spki':
			return { field: 'cert_spki_hash', op: 'eq', value };
		case 'script':
			return { field: 'script_hash', op: 'contains', value };
		default:
			return { field: 'cookie_name', op: 'contains', value };
	}
}
