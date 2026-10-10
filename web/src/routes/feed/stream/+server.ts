import { error } from '@sveltejs/kit';
import { apiURL } from '$lib/server/api';
import type { RequestHandler } from './$types';

/**
 * The live feed, proxied. On its own path, not beside the page: a `+server.ts` next to a `+page.svelte`
 * is resolved by the Accept header, so one URL would answer a page to a navigation and a stream to
 * anything else.
 *
 * The browser holds no credential, so its EventSource points here and this forwards with the
 * Authorization header, piping the body unbuffered (buffering turns a stream into one long silence).
 * Last-Event-ID is forwarded as it came: it is the cursor, so resuming needs nothing here.
 */
export const GET: RequestHandler = async ({ locals, url, fetch, request }) => {
	const token = locals.token!;

	const query = new URLSearchParams();
	// The cursor a first connection already knows. Reconnections use Last-Event-ID, which the browser
	// sends itself: that is the whole of the resumption.
	const cursor = url.searchParams.get('cursor');
	if (cursor) query.set('cursor', cursor);

	const headers: Record<string, string> = { authorization: 'Bearer ' + token };
	const lastEventID = request.headers.get('last-event-id');
	if (lastEventID) headers['last-event-id'] = lastEventID;

	const upstream = await fetch(apiURL() + '/feed?' + query.toString(), { headers });
	if (!upstream.ok || !upstream.body) {
		// Cancelled before throwing: EventSource reconnects on its own, so a token revoked upstream becomes
		// a refusal every few seconds, and an unread body is a connection nobody returns.
		await upstream.body?.cancel();
		error(upstream.status === 401 || upstream.status === 403 ? 403 : 502, 'the feed is unavailable');
	}

	return new Response(upstream.body, {
		status: 200,
		headers: {
			'content-type': 'text/event-stream',
			'cache-control': 'no-store',
			// Same reason as upstream: a proxy that buffers by default is the one
			// thing that silently breaks this.
			'x-accel-buffering': 'no'
		}
	});
};
