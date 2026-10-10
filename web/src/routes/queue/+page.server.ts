import { fail, get } from '$lib/server/api';
import type { QueueView } from '$lib/types';
import type { PageServerLoad } from './$types';

/**
 * The queue view. One call: programme names come from the layout's list, loaded for the switcher on
 * every page anyway, so this screen costs the control plane a single read.
 */
export const load: PageServerLoad = async ({ locals, fetch }) => {
	try {
		return { queue: await get<QueueView>(locals.token!, '/queue', fetch) };
	} catch (err) {
		fail(err);
	}
};
