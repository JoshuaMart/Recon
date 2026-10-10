import { error } from '@sveltejs/kit';
import { APIError, fail, get } from '$lib/server/api';
import type { Capabilities, Detail } from '$lib/types';
import type { PageServerLoad } from './$types';

/**
 * The asset view, the one screen whose data comes from `observation`. The list and facets never touch
 * the journal, a boundary asserted from both sides in the search layer's tests: a timeline of changes
 * is history, and the projection keeps a current state by construction.
 */
export const load: PageServerLoad = async ({ locals, params, fetch }) => {
	const token = locals.token!;

	try {
		const [detail, capabilities] = await Promise.all([
			get<Detail>(token, '/assets/' + encodeURIComponent(params.id), fetch),
			get<Capabilities>(token, '/assets/fields', fetch)
		]);
		return { detail, enriched: capabilities.enrichment?.configured ?? false };
	} catch (err) {
		// A 404 stays a 404. The server answers absence, not refusal, for another organization's asset, and
		// repeating that keeps the distinction from leaking through another status.
		if (err instanceof APIError && err.status === 404) {
			error(404, 'no such asset');
		}
		fail(err);
	}
};
