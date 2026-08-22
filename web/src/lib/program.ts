import { ago } from './format';
import { claimed } from './queue';
import type { Program, Run } from './types';

/**
 * An authorisation that has closed, or is about to.
 *
 * The automatic active to suspended transition at expiry comes with an alert
 * seven days out. Neither exists yet, so the screens say it rather than letting a
 * programme look active while its authorisation has run out.
 *
 * Here rather than in the list it was written for, because the detail makes the
 * same claim about the same date: two copies of a threshold this specific
 * diverge on the day somebody edits one of them.
 */
export function authorisation(
	to: string | undefined,
	now: number = Date.now()
): { label: string; tone: 'plain' | 'soon' | 'expired' } {
	if (!to) return { label: 'no end date', tone: 'plain' };
	const parsed = Date.parse(to);
	if (Number.isNaN(parsed)) return { label: 'no end date', tone: 'plain' };

	const days = (parsed - now) / 86400000;
	if (days < 0) return { label: 'expired ' + ago(to, now), tone: 'expired' };
	// Ceil, so the last hours of a permission read as one day and not as none.
	const left = Math.ceil(days);
	if (days < 7) return { label: `ends in ${left} ${left === 1 ? 'day' : 'days'}`, tone: 'soon' };
	return { label: 'until ' + to.slice(0, 10), tone: 'plain' };
}

export interface RunStatus {
	label: string;
	tone: 'idle' | 'running' | 'pending' | 'failed';
	/** Open, so no second run can be started for this program. */
	inFlight: boolean;
	/** Open and nothing has claimed it, which calls for the opposite reaction. */
	stalled: boolean;
}

/**
 * What the last discovery run of a program says about it.
 *
 * The distinction the whole thing turns on is the queue's: a run with no
 * `started_at` is not a scan to wait for, it is a provisioning that never reached
 * a scanner. `expired` is named rather than folded into "finished", because a run
 * the deadline sweeper closed produced nothing and a finished one did.
 */
export function runStatus(run: Run | null | undefined): RunStatus | null {
	if (!run) return null;

	const inFlight = run.state === 'pending' || run.state === 'running';
	if (inFlight) {
		return claimed(run.state, run.started_at)
			? { label: 'Scanning', tone: 'running', inFlight, stalled: false }
			: { label: 'Waiting for a scanner', tone: 'pending', inFlight, stalled: true };
	}
	if (run.state === 'failed') return { label: 'Last run failed', tone: 'failed', inFlight, stalled: false };
	if (run.state === 'expired') return { label: 'Last run expired', tone: 'failed', inFlight, stalled: false };
	return { label: 'Finished', tone: 'idle', inFlight, stalled: false };
}

/**
 * How much of what a program knows about, it is allowed to touch.
 *
 * The remainder is not one thing: an asset sits outside the perimeter because a
 * rule excludes it, or because no rule settles it at all and it is kept and never
 * probed. The bar draws the gap and says nothing about which half it is, which is
 * why the row spells both numbers out beside it.
 */
export function coverage(program: Program): number {
	const known = program.assets ?? 0;
	const inScope = program.assets_in_scope ?? 0;
	if (known <= 0) return 0;
	return Math.min(100, Math.max(0, Math.round((inScope / known) * 100)));
}
