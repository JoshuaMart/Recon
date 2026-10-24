import { api } from '#lib/api.ts';

/** The unseen changes, shown on the navigation and refreshed by whatever marks
 * some as seen. */
export const unseen = $state({ count: 0, capped: false });

export async function refreshUnseen() {
	try {
		const r = await api.get<{ unseen: number; capped: boolean }>('/api/changes/unseen');
		unseen.count = r.unseen;
		unseen.capped = r.capped;
	} catch {
		// The navigation keeps its last count.
	}
}
