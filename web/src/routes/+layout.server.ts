import { get } from '$lib/server/api';
import type { Program } from '$lib/types';
import type { LayoutServerLoad } from './$types';

/**
 * The programmes behind the topbar switcher. Loaded in the layout (the switcher is on every page) and
 * without counts, the one aggregation costing a scan. A failure costs the switcher, not the page.
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
