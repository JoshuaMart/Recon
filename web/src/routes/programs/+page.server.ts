import { redirect } from '@sveltejs/kit';
import { APIError, call, fail, get } from '$lib/server/api';
import type { Program } from '$lib/types';
import type { Actions, PageServerLoad } from './$types';

/** The programmes of the organisation , with the counts the screen asks for. */
export const load: PageServerLoad = async ({ locals, fetch }) => {
	try {
		const body = await get<{ programs: Program[] }>(locals.token!, '/programs?counts=1', fetch);
		return { programs: body.programs };
	} catch (err) {
		fail(err);
	}
};

export const actions: Actions = {
	/**
	 * Creating a program.
	 *
	 * It lands on the new program rather than back on the list, because a program
	 * with no scope rule discovers nothing: everything it finds falls into the third
	 * third state, kept and never probed. The next thing to do is add a rule, so the
	 * screen goes there.
	 */
	create: async ({ request, locals, fetch }) => {
		const form = await request.formData();

		const body: Record<string, unknown> = { name: String(form.get('name') ?? '').trim() };
		const ref = String(form.get('authorization_ref') ?? '').trim();
		if (ref) body.authorization_ref = ref;
		const platformRef = String(form.get('platform_ref') ?? '').trim();
		if (platformRef) body.platform_ref = platformRef;
		const until = String(form.get('authorized_to') ?? '').trim();
		// A date input gives a day, and an authorisation ends at the end of that day
		// rather than at midnight before it. Sending the bare date would cut the
		// permission a day early, which turns the program suspended.
		if (until) body.authorized_to = new Date(until + 'T23:59:59Z').toISOString();
		const rate = String(form.get('rate_limit_rps') ?? '').trim();
		if (rate) body.rate_limit_rps = Number(rate);

		let created: Program;
		try {
			created = await call<Program>(locals.token!, '/programs', body, fetch);
		} catch (err) {
			if (err instanceof APIError) {
				return { message: err.message };
			}
			return { message: err instanceof Error ? err.message : 'the program was not created' };
		}
		redirect(303, '/programs/' + created.id);
	}
};
