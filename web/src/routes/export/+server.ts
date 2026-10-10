import { fail, stream } from '$lib/server/api';
import { parseFilters, toAST } from '$lib/query';
import type { RequestHandler } from '@sveltejs/kit';

/**
 * The export, forwarded rather than fetched. It is the list's query (the same filters compile to the
 * same AST, so no "export query" to keep in step). The body is piped unbuffered, as the export must not
 * materialize an inventory first. A GET: a download started by following a link.
 */
export const GET: RequestHandler = async ({ locals, url, fetch }) => {
	const token = locals.token!;
	const filters = parseFilters(url.searchParams);
	const asked = url.searchParams.get('format');
	const format = asked === 'csv' || asked === 'urls' ? asked : 'jsonl';

	try {
		return await stream(token, '/assets/export', { filter: toAST(filters), format }, fetch);
	} catch (err) {
		fail(err);
	}
};
