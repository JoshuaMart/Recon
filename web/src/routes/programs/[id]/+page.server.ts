import { error } from '@sveltejs/kit';
import { APIError, call, fail, get, patch } from '$lib/server/api';
import { depthsByProgram, lastDiscoveryRuns, type QueueTotals } from '$lib/queue';
import type { Effect, ProgramDetail, QueueView, Run, ScopeRule } from '$lib/types';
import type { Actions, PageServerLoad } from './$types';

export const load: PageServerLoad = async ({ locals, params, fetch }) => {
	try {
		return {
			detail: await get<ProgramDetail>(locals.token!, '/programs/' + encodeURIComponent(params.id), fetch),
			...(await work(locals.token!, params.id, fetch))
		};
	} catch (err) {
		if (err instanceof APIError && err.status === 404) error(404, 'no such program');
		fail(err);
	}
};

/**
 * The last discovery run of this program, and what its queues hold.
 *
 * Read here rather than added to the program endpoint: the queue already answers
 * both, and a second place computing the same thing is a second place to keep in
 * step. Without it the panel only knows about a run for as long as the form
 * result lives, so a reload forgets one that is still open and the next click
 * meets a refusal with no warning.
 *
 * A failure costs the status line and the queue panel, not the page. Somebody who
 * cannot read the queue can still manage the perimeter. The two absences are
 * distinguishable on the screen because `reachable` says which one it is: no
 * answer from the queue is not a queue at rest.
 */
async function work(
	token: string,
	programID: string,
	fetcher: typeof fetch
): Promise<{ run: Run | null; depth: QueueTotals | null; reachable: boolean }> {
	try {
		const queue = await get<QueueView>(token, '/queue', fetcher);
		return {
			run: lastDiscoveryRuns(queue.runs).get(programID) ?? null,
			depth: depthsByProgram(queue.depths).get(programID) ?? null,
			reachable: true
		};
	} catch {
		return { run: null, depth: null, reachable: false };
	}
}

/**
 * The perimeter writes, as form actions.
 *
 * Every one of them carries the version it read. A 409 comes back as a message
 * and not as an error page: the caller did not make a mistake, it decided on a
 * state that has moved, and the only useful answer is to say so next to the form
 * so the page can be reloaded before writing again.
 */
export const actions: Actions = {
	addRule: async ({ request, locals, params, fetch }) => {
		const form = await request.formData();
		const note = String(form.get('note') ?? '');
		const body = {
			kind: String(form.get('kind') ?? ''),
			matcher: String(form.get('matcher') ?? ''),
			pattern: String(form.get('pattern') ?? ''),
			// Null rather than an empty string, so an unfilled field stays absent
			// rather than writing a note that says nothing.
			note: note || null
		};

		try {
			const answer = await call<{ rule: ScopeRule; effect: Effect }>(
				locals.token!,
				'/programs/' + encodeURIComponent(params.id) + '/rules',
				body,
				fetch
			);
			// The effect is the point of the answer: the reclassification committed
			// with the rule, so what moved is known now rather than at the next run.
			return { effect: answer.effect };
		} catch (err) {
			return refused(err);
		}
	},

	/**
	 * Closing a rule is setting its `valid_to`, and there is no delete anywhere on
	 * this screen because there is none on the surface behind it.
	 *
	 * The pattern is deliberately not sent. A close that restated the rule would be
	 * one keystroke away from rewriting one it only meant to end.
	 */
	closeRule: async ({ request, locals, params, fetch }) => {
		const form = await request.formData();
		const ruleID = String(form.get('rule_id') ?? '');
		const version = Number(form.get('version') ?? 0);

		try {
			const answer = await patch<{ rule: ScopeRule; effect: Effect }>(
				locals.token!,
				'/programs/' + encodeURIComponent(params.id) + '/rules/' + encodeURIComponent(ruleID),
				{ version, valid_to: new Date().toISOString() },
				fetch
			);
			return { effect: answer.effect };
		} catch (err) {
			return refused(err);
		}
	},

	/**
	 * Starting a run by hand, from the page it belongs to.
	 *
	 * What comes back is a run and, where no platform starts it, the arguments and
	 * environment it was to be started with. The page shows both. A screen
	 * answering "run started" would be saying something false in a local stack,
	 * where nothing launches the image and the row sits pending until the sweeper
	 * expires it.
	 */
	startRun: async ({ locals, params, fetch }) => {
		try {
			const run = await call<{
				started: boolean;
				run_id?: string;
				external_id?: string;
				reason?: string;
				args?: string[];
				env?: Record<string, string>;
			}>(locals.token!, '/programs/' + encodeURIComponent(params.id) + '/runs', { kind: 'discovery' }, fetch);
			return { run };
		} catch (err) {
			// Not `refused`. A 409 on a scope write is the optimistic lock, and the
			// page answers it with "reload before writing again". A 409 here is the
			// one-run-per-program bound, which no reload resolves and whose message
			// already names the run, its state and its age.
			return {
				message: err instanceof APIError ? err.message : 'the run could not be started',
				stale: false
			};
		}
	},

	updateProgram: async ({ request, locals, params, fetch }) => {
		const form = await request.formData();
		// The name travels on every edit because the write is a statement of the
		// program's state rather than a patch of the fields somebody touched. A
		// partial write against an optimistic lock is the shape where two edits
		// that never overlapped still lose each other.
		//
		// All of it, and the three at the bottom are the ones that were missing:
		// the server assigns every column unconditionally, so a field this form
		// left out came back as NULL. Changing a rate limit erased the platform
		// reference and the authorization reference, and nothing on this screen
		// could put them back.
		const text = (name: string) => String(form.get(name) ?? '') || null;
		const body: Record<string, unknown> = {
			version: Number(form.get('version') ?? 0),
			name: String(form.get('name') ?? ''),
			state: String(form.get('state') ?? 'active'),
			rate_limit_rps: Number(form.get('rate_limit_rps') ?? 0),
			discovery_interval: String(form.get('discovery_interval') ?? ''),
			authorized_from: String(form.get('authorized_from') ?? '') || undefined,
			authorized_to: text('authorized_to'),
			platform: text('platform'),
			platform_ref: text('platform_ref'),
			authorization_ref: text('authorization_ref')
		};

		try {
			await patch(locals.token!, '/programs/' + encodeURIComponent(params.id), body, fetch);
			return { saved: true };
		} catch (err) {
			return refused(err);
		}
	}
};

/** The shape a form renders, whatever went wrong. */
function refused(err: unknown): { message: string; stale: boolean } {
	if (err instanceof APIError) {
		// The machine readable half decides, not the status: a 409 on this surface
		// is always the lock, and saying "reload before writing again" to anything
		// else would be advice that does not apply.
		return { message: err.message, stale: err.reason === 'stale_version' };
	}
	return { message: err instanceof Error ? err.message : 'the write failed', stale: false };
}
