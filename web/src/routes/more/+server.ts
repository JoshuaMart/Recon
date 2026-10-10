import { call, fail } from '$lib/server/api';
import { isGrouped, parseFilters, toAST } from '$lib/query';
import type { FlatPage, GroupedPage } from '$lib/types';
import { json, type RequestHandler } from '@sveltejs/kit';

/**
 * One more page of the list, as JSON: the page's load query minus facets and capabilities. A
 * continuation narrows nothing, so its sidebar is the one on screen, and asking again would triple the
 * cost of a button that adds fifty rows. The credential is read from the cookie on this side.
 */
export const GET: RequestHandler = async ({ locals, url, fetch }) => {
	const token = locals.token!;
	const filters = parseFilters(url.searchParams);
	const cursor = url.searchParams.get('cursor') ?? undefined;
	// Two routes and not a flag, for the reason the page states: the two cursors
	// mean different columns, and one handed to the other is a refusal.
	const route = isGrouped(url.searchParams) ? '/assets/hosts' : '/assets/search';

	try {
		const page = await call<GroupedPage & FlatPage>(token, route, { filter: toAST(filters), cursor, limit: 50 }, fetch);
		return json(page);
	} catch (err) {
		fail(err);
	}
};
