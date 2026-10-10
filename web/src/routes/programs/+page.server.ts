import { redirect } from '@sveltejs/kit';
import { APIError, call, fail, get } from '$lib/server/api';
import type { Program, QueueDepth, QueueView, Run } from '$lib/types';
import type { Actions, PageServerLoad } from './$types';

/** The programmes of the organisation , with the counts the screen asks for. */
export const load: PageServerLoad = async ({ locals, fetch }) => {
	try {
		const body = await get<{ programs: Program[] }>(locals.token!, '/programs?counts=1', fetch);
		return { programs: body.programs, ...(await work(locals.token!, fetch)) };
	} catch (err) {
		fail(err);
	}
};

/**
 * What is running on each perimeter, and what is waiting. Read from the queue, not the program
 * endpoint, as the detail page does: a second place computing it is a second place to keep in step.
 *
 * A failure costs the two right-hand regions of every row, not the page. `reachable` is answered, not
 * inferred from empty lists: a queue that answered and holds nothing is a fresh organisation's ordinary
 * state, and reading it as unreachable would report an absence of measurement where there was one.
 */
async function work(
	token: string,
	fetcher: typeof fetch
): Promise<{ runs: Run[]; depths: QueueDepth[]; reachable: boolean }> {
	try {
		const queue = await get<QueueView>(token, '/queue', fetcher);
		return { runs: queue.runs, depths: queue.depths, reachable: true };
	} catch {
		return { runs: [], depths: [], reachable: false };
	}
}

export const actions: Actions = {
	/**
	 * Creating a program. It lands on the new program, not the list: a program with no scope rule
	 * discovers nothing (everything falls into the kept-and-never-probed state), so the next step is
	 * adding a rule and the screen goes there.
	 */
	create: async ({ request, locals, fetch }) => {
		const form = await request.formData();

		const body: Record<string, unknown> = { name: String(form.get('name') ?? '').trim() };
		const ref = String(form.get('authorization_ref') ?? '').trim();
		if (ref) body.authorization_ref = ref;
		const platformRef = String(form.get('platform_ref') ?? '').trim();
		if (platformRef) body.platform_ref = platformRef;
		const until = String(form.get('authorized_to') ?? '').trim();
		// A date input gives a day, and an authorisation ends at its end, not the midnight before: the
		// bare date would cut the permission a day early and suspend the program.
		if (until) body.authorized_to = new Date(until + 'T23:59:59Z').toISOString();
		const rate = String(form.get('rate_limit_rps') ?? '').trim();
		if (rate) body.rate_limit_rps = Number(rate);

		// The envelope, not the object. Every write here answers `{program: ...}` or
		// `{rule: ..., effect: ...}`, and reading one as the other is silent: `created.id` is undefined and
		// the redirect lands on /programs/undefined, where the server answers 404 for a programme that
		// was created.
		let created: { program: Program };
		try {
			created = await call<{ program: Program }>(locals.token!, '/programs', body, fetch);
		} catch (err) {
			if (err instanceof APIError) {
				return { message: err.message };
			}
			return { message: err instanceof Error ? err.message : 'the program was not created' };
		}
		redirect(303, '/programs/' + created.program.id);
	}
};
