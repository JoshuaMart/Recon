import { call, fail } from '$lib/server/api';
import { parseFilters, toAST } from '$lib/query';
import type { Facet } from '$lib/types';
import { json, type RequestHandler } from '@sveltejs/kit';

/**
 * One facet, opened on every value it has.
 *
 * The sidebar is capped at twenty values per field, which is what makes a
 * technology carried by twelve assets unreachable: it is in the inventory, it is
 * filterable, and there is nothing to click. This asks for the same aggregation
 * over the same filtered set, bounded higher, for the one field somebody opened.
 *
 * The filters travel, because a facet counts the filtered result and not the
 * inventory: opened without them the counts would disagree with the list beside
 * them.
 */
export const GET: RequestHandler = async ({ locals, url, fetch }) => {
	const token = locals.token!;
	const field = url.searchParams.get('field') ?? '';
	const filters = parseFilters(url.searchParams);

	try {
		const page = await call<{ facets: Facet[]; favicons?: Record<string, string> }>(
			token,
			'/assets/facets',
			{ filter: toAST(filters), field },
			fetch
		);
		return json(page);
	} catch (err) {
		fail(err);
	}
};
