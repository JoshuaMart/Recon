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
 * The last discovery run of this program, and what its queues hold. Read here, not added to the
 * program endpoint (the queue already answers both). Without it the panel knows a run only while the
 * form result lives, so a reload forgets one still open and the next click meets an unwarned refusal.
 * A failure costs the status line and queue panel, not the page; `reachable` tells the two absences
 * apart, as no answer is not a queue at rest.
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
 * The perimeter writes, as form actions. Each carries the version it read. A 409 returns as a
 * message, not an error page: the caller did not err, it decided on a state that moved, and the
 * useful answer is saying so by the form so the page is reloaded before writing again.
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
	 * Closing a rule sets its `valid_to`; there is no delete on this screen because none exists
	 * behind it. The pattern is deliberately not sent: a close restating the rule would be a
	 * keystroke from rewriting one it only meant to end.
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
	 * Starting a run by hand, from the page it belongs to. What comes back is a run and, where no
	 * platform starts it, the arguments and environment it was to be started with; the page shows
	 * both. "Run started" would be false on a local stack, where nothing launches the image and the
	 * row sits pending until the sweeper expires it.
	 */
	startRun: async ({ locals, params, fetch }) => {
		try {
			// A list, as the scanner takes one root domain per execution: three apexes are three runs.
			// `started` at the top is whether anything went out, and each entry says what happened to its own.
			const run = await call<{
				started: boolean;
				runs: {
					started: boolean;
					run_id: string;
					apex?: string;
					external_id?: string;
					reason?: string;
					args?: string[];
					env?: Record<string, string>;
				}[];
			}>(locals.token!, '/programs/' + encodeURIComponent(params.id) + '/runs', { kind: 'discovery' }, fetch);
			return { run };
		} catch (err) {
			// Not `refused`. A 409 on a scope write is the optimistic lock, answered with "reload before
			// writing again". Here it is the one-run-per-apex bound, which no reload resolves and whose
			// message already names the run, state and age.
			return {
				message: err instanceof APIError ? err.message : 'the run could not be started',
				stale: false
			};
		}
	},

	cancelRun: async ({ request, locals, fetch }) => {
		const form = await request.formData();
		const run = String(form.get('run') ?? '');
		const started = String(form.get('external') ?? '') !== '';
		try {
			const answer = await call<{ stopped: boolean }>(
				locals.token!,
				'/runs/' + encodeURIComponent(run) + '/cancel',
				{},
				fetch
			);
			// The run is closed either way. A stop the platform did not confirm
			// is said, because the execution may still be spending.
			if (started && !answer.stopped) {
				return {
					message: 'The run is closed, but the platform did not confirm the stop. Check the execution there.',
					stale: false
				};
			}
			return { cancelled: true };
		} catch (err) {
			// A 409 here is a run that finished or is finishing, and the message
			// says which. Not `refused`: no reload changes it.
			return {
				message: err instanceof APIError ? err.message : 'the run could not be cancelled',
				stale: false
			};
		}
	},

	updateProgram: async ({ request, locals, params, fetch }) => {
		const form = await request.formData();
		// The name travels on every edit because the write states the program's state, not a patch of touched
		// fields: a partial write against an optimistic lock is where two edits that never overlapped still
		// lose each other. The last three fields were once missing: the server assigns every column
		// unconditionally, so an omitted one came back NULL and a rate-limit change erased the platform and
		// authorization references.
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
		// The machine-readable half decides, not the status: a 409 here is always the lock, and "reload
		// before writing again" would be inapplicable advice for anything else.
		return { message: err.message, stale: err.reason === 'stale_version' };
	}
	return { message: err instanceof Error ? err.message : 'the write failed', stale: false };
}
