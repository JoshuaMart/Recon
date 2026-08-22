import { fail, get } from '$lib/server/api';
import type { QueueView } from '$lib/types';
import type { PageServerLoad } from './$types';

/**
 * The queue view.
 *
 * One call. The programme names come from the layout's own list, which is loaded
 * for the switcher on every page anyway, so this screen costs the control plane
 * a single read.
 */
export const load: PageServerLoad = async ({ locals, fetch }) => {
	try {
		return { queue: await get<QueueView>(locals.token!, '/queue', fetch) };
	} catch (err) {
		fail(err);
	}
};
