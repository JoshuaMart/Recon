/**
 * Filters, in the URL and as an AST. The structured representation comes first and the query language
 * later, so nothing parses a DSL: the interface builds a conjunction of facets whose selected values
 * are alternatives, carried as repeated `f` params so a filtered list is a shareable link rendered on
 * the server. The language will fill this same tree as a second way in.
 */

export type Op = 'eq' | 'in' | 'suffix' | 'prefix' | 'contains' | 'exists' | 'gt' | 'gte' | 'lt' | 'lte';

export interface Filter {
	field: string;
	op: Op;
	value: string;
}

/**
 * Which fields need a non-string value. A second registry beside the Go one, kept small: a facet term
 * is cast to text by the aggregation (port 443 returns as "443") and the compiler refuses a string for
 * an int column. Everything absent travels as text, `program_id` included (the server casts it).
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
 * Every link the interface builds is built here: the one place a base path would be applied,
 * keeping components free of URL assembly, where an unescaped value slips into a query string.
 */

/**
 * programHref is the list filtered to one perimeter, linked from the programmes screen.
 * `program_id` is a field where `org_id` is not: inside one organization, asking what a
 * perimeter holds is legitimate, and the tenant clause is emitted beside it regardless.
 */
export function programHref(programID: string): string {
	return href([{ field: 'program_id', op: 'eq', value: programID }]);
}

/**
 * Which shape the list takes, read from the URL. Grouped is the default, so the parameter names the
 * exception: `group=none` is the flat list, mostly one host written five times. The shapes are two
 * server routes, not a flag, as their cursors mean different columns; every link below drops the
 * cursor when it changes shape.
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
 * groupHref toggles the fold, keeping the filters and dropping the cursor. Dropping it is the
 * point: a flat cursor walks assets and a grouped one walks hosts, so carrying one across would
 * ask the server to continue a walk it never started. The server refuses it; this makes the
 * refusal unreachable from the interface.
 */
export function groupHref(filters: Filter[], grouped: boolean): string {
	return href(filters, grouped);
}

/**
 * nextHref continues the list with a cursor, never a page number: the server chose the ordering, and
 * an offset over a million rows rescans everything before it. It is the load button's no-JavaScript
 * fallback, where replacing the page beats a dead control.
 */
export function nextHref(filters: Filter[], cursor: string, grouped = true): string {
	const search = params(filters);
	if (!grouped) search.set('group', 'none');
	search.set('cursor', cursor);
	return '/?' + search.toString();
}

/** moreHref is the same page as `nextHref`, asked for as JSON so the rows can be
 *  appended to the ones already on screen instead of replacing them. */
export function moreHref(filters: Filter[], cursor: string, grouped = true): string {
	const search = params(filters);
	if (!grouped) search.set('group', 'none');
	search.set('cursor', cursor);
	return '/more?' + search.toString();
}

/**
 * The field the search box writes into, and its operator. `key`, not `host`: the key is the asset's
 * name as the list shows it (host:port for a service, the URL for a page). `contains` is what a search
 * field means; no index answers it, so the server grants it on the name alone.
 */
export const searchField = 'key';
const searchOp: Op = 'contains';

/** The term currently in the box, so a reload shows the search that is in force. */
export function searchTerm(filters: Filter[]): string {
	return filters.find((filter) => filter.field === searchField && filter.op === searchOp)?.value ?? '';
}

/**
 * withSearch replaces the term, not adds one: two substrings of one name is not a question
 * anyone asks by typing twice in a box, and an accumulating box would need its own undo. Other
 * filters are untouched; a search narrows what the facets chose.
 */
export function withSearch(filters: Filter[], term: string): Filter[] {
	const rest = filters.filter((filter) => !(filter.field === searchField && filter.op === searchOp));
	const value = term.trim();
	if (!value) return rest;
	return [...rest, { field: searchField, op: searchOp, value }];
}

/** searchHref is the list narrowed to a name, dropping the cursor like every
 *  other change of the question. */
export function searchHref(filters: Filter[], term: string, grouped = true): string {
	return href(withSearch(filters, term), grouped);
}

/**
 * facetHref opens one facet on every value it has, as JSON. Filters from other fields travel,
 * this field's do not: a selected status must not hide the other statuses, as its facet answers
 * the choices left under the rest of the question. The usual disjunctive-facet shape:
 * `(status = 200 OR status = 302) AND kind = service`.
 */
export function facetHref(filters: Filter[], field: string): string {
	const search = params(withoutField(filters, field));
	search.set('field', field);
	return '/facet?' + search.toString();
}

/** exportHref is the same query as the list, by construction. */
export type ExportFormat = 'jsonl' | 'csv' | 'urls';

export function exportHref(filters: Filter[], format: ExportFormat = 'jsonl'): string {
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

/** Remove every selection made in one field while preserving the other facets. */
export function withoutField(filters: Filter[], field: string): Filter[] {
	return filters.filter((filter) => filter.field !== field);
}

/**
 * The groups the facet UI presents. Repeated equality and containment selections in one field
 * are alternatives. Range and text-shape operators stay separate constraints: ORing
 * `port >= 80` and `port <= 443` would silently widen the query.
 */
export function filterGroups(filters: Filter[]): Filter[][] {
	const groups: Filter[][] = [];
	const alternatives = new Map<string, number>();

	for (const filter of filters) {
		if (filter.op === 'eq' || filter.op === 'contains') {
			const key = `${filter.field}:${filter.op}`;
			const index = alternatives.get(key);
			if (index !== undefined) {
				groups[index].push(filter);
				continue;
			}
			alternatives.set(key, groups.length);
		}
		groups.push([filter]);
	}
	return groups;
}

/**
 * toAST compiles facet selections into the tree the API expects. Values selected in one facet
 * are ORed; different facets and every other constraint are ANDed. No organization clause can
 * appear: `org_id` is not in the server's registry, so the control plane emits it outside the
 * tree.
 */
export function toAST(filters: Filter[]): Node | undefined {
	if (filters.length === 0) return undefined;

	const leaf = (filter: Filter): Node => ({
		field: filter.field,
		op: filter.op,
		value: typed(filter.field, filter.value)
	});
	const clauses: Node[] = filterGroups(filters).map((group) => {
		const alternatives = group.map(leaf);
		return alternatives.length === 1 ? alternatives[0] : { op: 'or', clauses: alternatives };
	});

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
	// An identifier is what the query needs and a name what a reader needs: the chip shows the
	// resolved name, or the identifier, never nothing.
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
 * facetFilter builds the filter a facet bucket links to, with the operator the server says the field
 * takes. The sidebar once wrote `eq` for every bucket, wrong for an array (`technologies does not
 * accept "eq"`, only after a click); operators come from `GET /assets/fields`. `eq` when accepted,
 * else `contains`, else `eq` so a missing field still yields a filter the server can name.
 */
export function facetFilter(field: string, value: string, operators: Record<string, string[]>): Filter {
	const allowed = operators[field] ?? [];
	if (allowed.includes('eq') || allowed.length === 0) return { field, op: 'eq', value };
	if (allowed.includes('contains')) return { field, op: 'contains', value };
	return { field, op: allowed[0] as Filter['op'], value };
}

/**
 * badgeFilter maps a pivot type back to the field that searches it. The vocabularies differ
 * and the mapping belongs here, not in a component: internal/search names the pivot `favicon`
 * and the field `favicon_hash`, and a value keyed on the wrong one silently matches nothing.
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
