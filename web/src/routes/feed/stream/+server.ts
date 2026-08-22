import { error } from '@sveltejs/kit';
import { apiURL } from '$lib/server/api';
import type { RequestHandler } from './$types';

/**
 * The live feed, proxied.
 *
 * On its own path rather than beside the page, and that is not tidiness. A
 * `+server.ts` sitting next to a `+page.svelte` is resolved by the Accept header,
 * so the same URL answers a page to a browser navigation and a stream to anything
 * else. Two things behind one address, chosen by a header nobody set on purpose,
 * is a distinction that holds until the first client that accepts anything.
 *
 * The browser holds no credential, so the EventSource it opens points here and
 * this forwards with the Authorization header. The upstream body is piped through
 * untouched: a proxy that buffered would turn a stream into one long silence
 * followed by everything at once, which is the failure this endpoint exists to
 * avoid.
 *
 * Last-Event-ID is forwarded as it came. It is the cursor, so the resume works
 * without this layer knowing what a cursor is.
 */
export const GET: RequestHandler = async ({ locals, url, fetch, request }) => {
	const token = locals.token!;

	const query = new URLSearchParams();
	// The cursor a first connection already knows. Reconnections do not use it:
	// the browser sends Last-Event-ID by itself, and that is the whole of the
	// resumption.
	const cursor = url.searchParams.get('cursor');
	if (cursor) query.set('cursor', cursor);

	const headers: Record<string, string> = { authorization: 'Bearer ' + token };
	const lastEventID = request.headers.get('last-event-id');
	if (lastEventID) headers['last-event-id'] = lastEventID;

	const upstream = await fetch(apiURL() + '/feed?' + query.toString(), { headers });
	if (!upstream.ok || !upstream.body) {
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
