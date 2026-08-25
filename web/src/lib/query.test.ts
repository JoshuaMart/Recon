import { describe, expect, it } from 'vitest';
import {
	badgeFilter,
	facetFilter,
	groupHref,
	href,
	isGrouped,
	label,
	moreHref,
	nextHref,
	parseFilters,
	searchHref,
	searchTerm,
	toAST,
	withFilter,
	withoutFilter,
	withSearch
} from './query';
import type { Filter } from './query';

describe('parseFilters', () => {
	it('reads a filter whose value contains a colon', () => {
		// A service key is host:port and a lineage string is full of colons, so
		// splitting on every colon would corrupt exactly the values this interface
		// puts in the URL.
		const filters = parseFilters(new URLSearchParams('f=key:eq:a.example.com:8443'));
		expect(filters).toEqual([{ field: 'key', op: 'eq', value: 'a.example.com:8443' }]);
	});

	it('drops what it cannot read rather than guessing', () => {
		const filters = parseFilters(new URLSearchParams('f=nonsense&f=:eq:x&f=port:eq:&f=port:eq:443'));
		expect(filters).toEqual([{ field: 'port', op: 'eq', value: '443' }]);
	});
});

describe('toAST', () => {
	it('is undefined with no filter, which is a legitimate first page', () => {
		expect(toAST([])).toBeUndefined();
	});

	it('types a facet bucket back into what the column holds', () => {
		// The reason this module has a registry at all: a facet casts its buckets
		// to text, so port 443 comes back as the string "443" and the AST refuses a
		// string where the column is an int.
		expect(toAST([{ field: 'port', op: 'eq', value: '443' }])).toEqual({
			field: 'port',
			op: 'eq',
			value: 443
		});
		expect(toAST([{ field: 'is_cdn', op: 'eq', value: 'false' }])).toEqual({
			field: 'is_cdn',
			op: 'eq',
			value: false
		});
		expect(toAST([{ field: 'asn_org', op: 'eq', value: 'OVH SAS' }])).toEqual({
			field: 'asn_org',
			op: 'eq',
			value: 'OVH SAS'
		});
	});

	it('leaves a numeric field alone when the value is not a whole number', () => {
		// Better a refusal from the server, which names the clause, than a silent
		// NaN travelling as a parameter.
		expect(toAST([{ field: 'port', op: 'eq', value: 'https' }])).toEqual({
			field: 'port',
			op: 'eq',
			value: 'https'
		});
	});

	it('ands several filters and never emits an organisation clause', () => {
		const tree = toAST([
			{ field: 'key', op: 'suffix', value: '.jomar.ovh' },
			{ field: 'lifecycle', op: 'eq', value: 'active' }
		]);
		expect(tree).toEqual({
			op: 'and',
			clauses: [
				{ field: 'key', op: 'suffix', value: '.jomar.ovh' },
				{ field: 'lifecycle', op: 'eq', value: 'active' }
			]
		});
		// the compiler: org_id is not a field, so the tree cannot carry it either way.
		// The control plane emits it outside the tree on every compilation.
		expect(JSON.stringify(tree)).not.toContain('org');
	});
});

describe('withFilter', () => {
	it('does not add the same filter twice', () => {
		const one = [{ field: 'port' as const, op: 'eq' as const, value: '443' }];
		expect(withFilter(one, { field: 'port', op: 'eq', value: '443' })).toHaveLength(1);
		expect(withFilter(one, { field: 'port', op: 'eq', value: '80' })).toHaveLength(2);
	});

	it('removes only the one asked for', () => {
		const two = [
			{ field: 'port' as const, op: 'eq' as const, value: '443' },
			{ field: 'port' as const, op: 'eq' as const, value: '80' }
		];
		expect(withoutFilter(two, two[0])).toEqual([two[1]]);
	});
});

describe('badgeFilter', () => {
	it('maps a pivot type onto the field that searches it', () => {
		// The two vocabularies differ: internal/search names the type `favicon` and
		// the searchable field `favicon_hash`. Keyed on the wrong one, a badge is a
		// link that silently matches nothing.
		expect(badgeFilter('favicon', 'abc')).toEqual({ field: 'favicon_hash', op: 'eq', value: 'abc' });
		expect(badgeFilter('cert_spki', 'abc')).toEqual({ field: 'cert_spki_hash', op: 'eq', value: 'abc' });
		expect(badgeFilter('script', 'abc')).toEqual({ field: 'script_hash', op: 'contains', value: 'abc' });
		expect(badgeFilter('cookie_name', 'SESS')).toEqual({
			field: 'cookie_name',
			op: 'contains',
			value: 'SESS'
		});
	});
});

describe('href and label', () => {
	it('round trips a filter through the URL', () => {
		const filters = [{ field: 'key' as const, op: 'suffix' as const, value: '.jomar.ovh' }];
		const link = href(filters);
		expect(parseFilters(new URLSearchParams(link.slice(2)))).toEqual(filters);
	});

	it('drops the cursor, since a new filter is a new first page', () => {
		expect(href([])).toBe('/');
		expect(href([{ field: 'port', op: 'eq', value: '443' }])).not.toContain('cursor');
	});

	it('says the operator in words rather than in symbols', () => {
		expect(label({ field: 'key', op: 'suffix', value: '.jomar.ovh' })).toBe('name ends with .jomar.ovh');
		expect(label({ field: 'technologies', op: 'contains', value: 'nginx' })).toBe('technologies includes nginx');
		expect(label({ field: 'is_cdn', op: 'eq', value: 'true' })).toBe('fronted is true');
	});
});

describe('facetFilter', () => {
	// The defect this exists for: the sidebar wrote `eq` for every bucket, and an
	// array column takes `contains`. It came back as
	// `technologies does not accept "eq"`, and only once somebody clicked one.
	const operators = {
		technologies: ['contains', 'exists'],
		country: ['eq', 'neq', 'in', 'exists'],
		favicon_hash: ['eq', 'neq', 'in', 'exists']
	};

	it('uses contains on a field that refuses eq', () => {
		expect(facetFilter('technologies', 'nginx', operators)).toEqual({
			field: 'technologies',
			op: 'contains',
			value: 'nginx'
		});
	});

	it('uses eq where the field takes it', () => {
		expect(facetFilter('country', 'FR', operators).op).toBe('eq');
	});

	// A field the server did not describe still produces a filter: a 400 naming the
	// field is worth more than a link that does nothing.
	it('falls back to eq on an unknown field', () => {
		expect(facetFilter('whatever', 'x', operators).op).toBe('eq');
	});

	// And a field that takes neither is not silently dropped either.
	it('takes the first operator when neither eq nor contains is allowed', () => {
		expect(facetFilter('odd', 'x', { odd: ['exists'] }).op).toBe('exists');
	});
});

describe('isGrouped and the shape of the list', () => {
	// the fold inverts the default: the inventory is 98 assets over 33 hosts, so the
	// flat list is mostly one host written five times. The parameter therefore names the
	// exception, and an URL that carries none is the grouped list.
	it('is grouped unless the URL says otherwise', () => {
		expect(isGrouped(new URLSearchParams(''))).toBe(true);
		expect(isGrouped(new URLSearchParams('f=port:eq:443'))).toBe(true);
		expect(isGrouped(new URLSearchParams('group=none'))).toBe(false);
	});

	it('writes the exception into the link and nothing into the default', () => {
		const filters: Filter[] = [{ field: 'port', op: 'eq', value: '443' }];
		expect(href(filters)).toBe('/?f=port%3Aeq%3A443');
		expect(href(filters, false)).toBe('/?f=port%3Aeq%3A443&group=none');
		expect(groupHref(filters, true)).toBe('/?f=port%3Aeq%3A443');
	});

	// Dropping the cursor across the toggle is the point rather than an omission: a
	// cursor of the flat list walks assets and a cursor of the grouped one walks hosts.
	it('carries the shape into the next page and never the cursor across the toggle', () => {
		const filters: Filter[] = [];
		expect(nextHref(filters, 'abc')).toBe('/?cursor=abc');
		expect(nextHref(filters, 'abc', false)).toBe('/?group=none&cursor=abc');
		expect(groupHref(filters, false)).toBe('/?group=none');
	});
});

describe('the list shape survives a link', () => {
	const filters = [{ field: 'port', op: 'eq' as const, value: '443' }];

	// The shape lives in the URL, so every link built from it has to carry it.
	// It defaults to grouped, which is right for a link arriving from elsewhere
	// and wrong for every link on the list itself: from the flat list, adding a
	// facet folded it back without anybody asking.
	it('keeps the flat list flat', () => {
		expect(href(filters, false)).toContain('group=none');
		expect(href([], false)).toBe('/?group=none');
	});

	it('keeps the grouped list grouped, and says so by omission', () => {
		expect(href(filters, true)).not.toContain('group=');
		expect(href([])).toBe('/');
	});

	// Dropping the cursor when the shape changes is the point rather than an
	// omission: the two walk different keys, so carrying one across would ask
	// the server to continue a walk it never started.
	it('drops the cursor on both sides of the toggle', () => {
		expect(groupHref(filters, false)).not.toContain('cursor');
		expect(groupHref(filters, true)).not.toContain('cursor');
	});
});

describe('the search box', () => {
	// The box writes a filter and nothing else, so a search is undone by the same
	// chip as a facet click and travels in the same shareable URL.
	it('narrows the name and keeps every other filter', () => {
		const filters: Filter[] = [{ field: 'port', op: 'eq', value: '443' }];
		expect(withSearch(filters, 'admin')).toEqual([
			{ field: 'port', op: 'eq', value: '443' },
			{ field: 'key', op: 'contains', value: 'admin' }
		]);
	});

	// Typing again is a new search rather than a second one. Two substrings of one
	// name is not a question anybody asks by typing twice in the same box, and a
	// box that accumulated would need its own way to undo.
	it('replaces the term rather than adding one', () => {
		const searched = withSearch([], 'admin');
		expect(withSearch(searched, 'staging')).toEqual([{ field: 'key', op: 'contains', value: 'staging' }]);
	});

	// Emptying the box is how a search is cleared, so it has to remove the filter
	// rather than search for nothing.
	it('clears the filter on an empty term', () => {
		expect(withSearch(withSearch([], 'admin'), '  ')).toEqual([]);
		expect(searchHref([], '')).toBe('/');
	});

	// The field shows the search in force, read from the URL rather than kept as
	// state: the back button and the chip in the toolbar both have to move it.
	it('reads the term back out of the filters', () => {
		expect(searchTerm(withSearch([], 'admin'))).toBe('admin');
		expect(searchTerm([{ field: 'key', op: 'eq', value: 'a.target.test' }])).toBe('');
	});

	// A search is a change of the question, so it starts the list again rather
	// than continuing a walk that was ordered by something else.
	it('drops the cursor and keeps the shape', () => {
		expect(searchHref([], 'admin', false)).toBe('/?f=key%3Acontains%3Aadmin&group=none');
		expect(searchHref([], 'admin')).not.toContain('cursor');
	});
});

describe('the continuation', () => {
	// The button appends rather than navigates, and the JSON it asks for is the
	// same page as the link it falls back to without JavaScript.
	it('asks for the same page as the link, as JSON', () => {
		const filters: Filter[] = [{ field: 'port', op: 'eq', value: '443' }];
		expect(moreHref(filters, 'abc')).toBe('/more?f=port%3Aeq%3A443&cursor=abc');
		expect(moreHref(filters, 'abc', false)).toBe('/more?f=port%3Aeq%3A443&group=none&cursor=abc');
		expect(moreHref(filters, 'abc').slice('/more'.length)).toBe(nextHref(filters, 'abc').slice('/'.length));
	});
});
