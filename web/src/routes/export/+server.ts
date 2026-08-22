import { fail, stream } from '$lib/server/api';
import { parseFilters, toAST } from '$lib/query';
import type { RequestHandler } from '@sveltejs/kit';

/**
 * The export, forwarded rather than fetched.
 *
 * It is the same query as the list: the same filters compile to the same AST, and
 * there is no "export query" to keep in step with the one people actually use.
 *
 * The body is piped through without being buffered. The export is forbidden from materializing an
 * inventory before sending it, and a proxy that accumulated would undo that
 * property at the one place it was built. A GET because it is a download the
 * browser starts by following a link.
 */
export const GET: RequestHandler = async ({ locals, url, fetch }) => {
	const token = locals.token!;
	const filters = parseFilters(url.searchParams);
	const format = url.searchParams.get('format') === 'csv' ? 'csv' : 'jsonl';

	try {
		return await stream(token, '/assets/export', { filter: toAST(filters), format }, fetch);
	} catch (err) {
		fail(err);
	}
};
