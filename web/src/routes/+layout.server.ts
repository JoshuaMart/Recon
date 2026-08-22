import { get } from '$lib/server/api';
import type { Program } from '$lib/types';
import type { LayoutServerLoad } from './$types';

/**
 * The programmes behind the switcher in the topbar.
 *
 * Loaded in the layout because the switcher is on every page, and asked for without
 * counts: the per-program asset counts are the one aggregation that costs a scan
 * 5, and putting it in front of every search would be paying for it on pages that
 * show nothing of it.
 *
 * A failure costs the switcher and not the page. Somebody who cannot list the
 * programmes can still read the inventory.
 */
export const load: LayoutServerLoad = async ({ locals, url, fetch }) => {
	if (!locals.token || url.pathname === '/connect') return { programs: [] };

	try {
		const body = await get<{ programs: Program[] }>(locals.token, '/programs', fetch);
		return { programs: body.programs };
	} catch {
		return { programs: [] };
	}
};
