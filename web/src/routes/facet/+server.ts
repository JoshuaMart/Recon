import { call, fail } from '$lib/server/api';
import { parseFilters, toAST, withoutField } from '$lib/query';
import type { Facet } from '$lib/types';
import { json, type RequestHandler } from '@sveltejs/kit';

/**
 * One facet, opened on every value. The sidebar caps at twenty values per field, so a technology on
 * twelve assets was filterable but unreachable; this asks the same aggregation over the same set,
 * bounded higher, for the one field opened. It removes the chosen field's own filter, so opening
 * status after choosing 200 still returns 302.
 */
export const GET: RequestHandler = async ({ locals, url, fetch }) => {
	const token = locals.token!;
	const field = url.searchParams.get('field') ?? '';
	const filters = parseFilters(url.searchParams);

	try {
		const page = await call<{ facets: Facet[]; favicons?: Record<string, string> }>(
			token,
			'/assets/facets',
			{ filter: toAST(withoutField(filters, field)), field },
			fetch
		);
		return json(page);
	} catch (err) {
		fail(err);
	}
};
