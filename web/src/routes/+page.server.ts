import { call, fail, get } from '$lib/server/api';
import { href, isGrouped, parseFilters, toAST, withSearch, type Filter } from '$lib/query';
import type { Capabilities, Facet, FlatPage, GroupedPage, Program } from '$lib/types';
import { redirect } from '@sveltejs/kit';
import type { PageServerLoad } from './$types';

/**
 * The search view.
 *
 * Three calls, in parallel, because none needs another's answer: the facets are an
 * aggregation over the same filtered set as the list, and the capabilities depend
 * on neither. In sequence this would pay the filter twice in wall clock for no
 * reason.
 */
export const load: PageServerLoad = async ({ locals, url, fetch }) => {
	const token = locals.token!;
	const filters = parseFilters(url.searchParams);

	/**
	 * The search box submits a typed word, and this turns it into the filter it
	 * means before anything renders.
	 *
	 * A redirect rather than a second way of filtering: `q` never survives this
	 * function, so the list has exactly one representation of its question, the
	 * chip in the toolbar knows how to remove it, and the URL somebody copies is
	 * the same one a facet click produces. It is also what makes the box work
	 * with no JavaScript, where a form submission is all there is.
	 */
	if (url.searchParams.has('q')) {
		redirect(303, href(withSearch(filters, url.searchParams.get('q') ?? ''), isGrouped(url.searchParams)));
	}

	const filter = toAST(filters);
	const cursor = url.searchParams.get('cursor') ?? undefined;
	// Grouped is the list and flat is the exception, so the parameter names the
	// exception. In the URL like every other piece of list state, so both shapes
	// are a link somebody can send and the back button means something.
	const grouped = isGrouped(url.searchParams);
	// Two routes and not a flag, because the two cursors mean different columns.
	// Sending one to the other is a refusal on the server, and the links here
	// drop the cursor when they change shape so it is unreachable from the page.
	const route = grouped ? '/assets/hosts' : '/assets/search';

	try {
		const [page, facets, capabilities] = await Promise.all([
			call<GroupedPage & FlatPage>(token, route, { filter, cursor, limit: 50 }, fetch),
			call<{ facets: Facet[]; favicons?: Record<string, string> }>(token, '/assets/facets', { filter }, fetch),
			// Whether this deployment enriches at all. Asked of the server because
			// the console cannot see the difference between "no MaxMind database"
			// and "no match": both give zero ASN, and an empty infrastructure
			// family reads as a broken interface rather than as a normal
			// deployment.
			get<Capabilities>(token, '/assets/fields', fetch)
		]);

		return {
			filters,
			grouped,
			groups: page.groups ?? [],
			assets: page.assets ?? [],
			// The two maps merged, and the order matters in one direction only: the
			// facet answer covers the sidebar's icons and the page answer covers the
			// rows, and a hash in both carries the same image because it is the hash
			// of those bytes. The sidebar ranks the whole filtered result, so its
			// most shared icon is routinely one no row on screen carries.
			favicons: { ...(facets.favicons ?? {}), ...(page.favicons ?? {}) },
			nextCursor: page.next_cursor,
			facets: facets.facets,
			enriched: capabilities.enrichment?.configured ?? false,
			// The sidebar links to a filter, and the operator a field takes is the
			// server's to say. Guessed here, an array field comes back as a 400
			// after somebody clicked it.
			operators: capabilities.fields ?? {},
			// Only when a program filter is present, which is what makes this free
			// on every other page load.
			programNames: await programNames(token, filters, fetch)
		};
	} catch (err) {
		fail(err);
	}
};

/**
 * The names behind the program identifiers a filter carries.
 *
 * A chip reading "program is 3f9a…" is a chip nobody can check. The identifier is
 * what the query needs and the name is what a reader needs, and resolving it here
 * keeps the URL shareable: a link carrying a name would break the moment somebody
 * renamed a program.
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
