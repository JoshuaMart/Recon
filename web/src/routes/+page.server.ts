import { call, fail, get } from '$lib/server/api';
import { href, isGrouped, parseFilters, toAST, withSearch, withoutField, type Filter } from '$lib/query';
import type { Capabilities, Facet, FlatPage, GroupedPage, Program } from '$lib/types';
import { redirect } from '@sveltejs/kit';
import type { PageServerLoad } from './$types';

/**
 * The search view. The list, sidebar and capabilities start in parallel. When a facet is
 * selected, its own values are refreshed without that field's filter, keeping the other choices
 * visible: disjunctive-facet semantics.
 */
export const load: PageServerLoad = async ({ locals, url, fetch }) => {
	const token = locals.token!;
	const filters = parseFilters(url.searchParams);

	/**
	 * The search box submits a typed word, and this turns it into the filter it means before anything
	 * renders. A redirect, not a second way of filtering: `q` never survives, so the list has one
	 * representation of its question, the toolbar chip knows how to remove it, and the copied URL is
	 * the one a facet click produces. It also makes the box work without JavaScript.
	 */
	if (url.searchParams.has('q')) {
		redirect(303, href(withSearch(filters, url.searchParams.get('q') ?? ''), isGrouped(url.searchParams)));
	}

	const filter = toAST(filters);
	const cursor = url.searchParams.get('cursor') ?? undefined;
	// Grouped is the list and flat the exception, so the parameter names the exception. In the URL
	// like all list state: both shapes are sendable links and the back button means something.
	const grouped = isGrouped(url.searchParams);
	// Two routes, not a flag: the cursors mean different columns. Sending one to the other is refused
	// by the server, and links here drop the cursor on a shape change so it is unreachable.
	const route = grouped ? '/assets/hosts' : '/assets/search';

	try {
		const [page, sidebar, capabilities, names] = await Promise.all([
			call<GroupedPage & FlatPage>(token, route, { filter, cursor, limit: 50 }, fetch),
			call<FacetAnswer>(token, '/assets/facets', { filter }, fetch),
			// Whether this deployment enriches at all. Asked of the server: the console cannot tell "no
			// MaxMind database" from "no match" (both zero ASN), and an empty infrastructure family reads as a
			// broken interface, not a normal deployment.
			get<Capabilities>(token, '/assets/fields', fetch),
			programNames(token, filters, fetch)
		]);
		const facets = await disjunctiveFacets(token, filters, sidebar, fetch);

		return {
			filters,
			grouped,
			groups: page.groups ?? [],
			assets: page.assets ?? [],
			// The two maps merged; order matters one way only. The facet answer covers the sidebar's icons and
			// the page answer the rows, and a hash in both has the same image (the hash of those bytes). The
			// sidebar ranks the whole filtered result, so its most shared icon is often one no row carries.
			favicons: { ...(facets.favicons ?? {}), ...(page.favicons ?? {}) },
			nextCursor: page.next_cursor,
			facets: facets.facets,
			enriched: capabilities.enrichment?.configured ?? false,
			// The sidebar links to a filter, and the operator a field takes is the server's to say; guessed
			// here, an array field returns a 400 after a click.
			operators: capabilities.fields ?? {},
			programNames: names
		};
	} catch (err) {
		fail(err);
	}
};

interface FacetAnswer {
	facets: Facet[];
	favicons?: Record<string, string>;
}

/**
 * Refresh each selected facet without its own selections. The list still gets the complete
 * filter; only the active facet's counters exclude themselves, so after choosing 200 the sidebar
 * still offers 302 with every other condition in force.
 */
async function disjunctiveFacets(
	token: string,
	filters: Filter[],
	sidebar: FacetAnswer,
	fetcher: typeof fetch
): Promise<FacetAnswer> {
	const selected = new Set(
		filters.filter((filter) => filter.op === 'eq' || filter.op === 'contains').map((filter) => filter.field)
	);
	const fields = [...new Set(sidebar.facets.map((facet) => facet.field).filter((field) => selected.has(field)))];
	if (fields.length === 0) return sidebar;

	const answers = await Promise.all(
		fields.map((field) =>
			call<FacetAnswer>(token, '/assets/facets', { filter: toAST(withoutField(filters, field)), field }, fetcher)
		)
	);
	const replacements = new Map(
		answers.flatMap((answer) => answer.facets).map((facet) => [facet.field, facet] as const)
	);

	return {
		facets: sidebar.facets.map((facet) => replacements.get(facet.field) ?? facet),
		favicons: Object.assign({}, sidebar.favicons, ...answers.map((answer) => answer.favicons ?? {}))
	};
}

/**
 * The names behind the program identifiers a filter carries. A chip reading "program is 3f9a…"
 * cannot be checked. The query needs the identifier and a reader the name; resolving here keeps
 * the URL shareable, as a link carrying a name breaks when a program is renamed.
 */
async function programNames(token: string, filters: Filter[], fetcher: typeof fetch): Promise<Record<string, string>> {
	if (!filters.some((filter) => filter.field === 'program_id')) return {};

	try {
		const body = await get<{ programs: Program[] }>(token, '/programs', fetcher);
		return Object.fromEntries(body.programs.map((program) => [program.id, program.name]));
	} catch {
		// A name is decoration. Losing it must not lose the search, so the chip
		// falls back to the identifier it already has.
		return {};
	}
}
