import { ago } from './format';
import { claimed } from './queue';
import type { Program, Run } from './types';

/**
 * An authorisation that has closed, or is about to. The automatic active-to-suspended transition at
 * expiry, and a seven-day alert, do not exist yet, so the screens say it rather than let a programme
 * look active with its authorisation run out. Shared with the detail, which makes the same claim.
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
 * What a program's last discovery run says about it. The queue's distinction decides: a run with
 * no `started_at` is a provisioning that never reached a scanner, not a scan to wait for.
 * `expired` is named, not folded into "finished": a run the deadline sweeper closed produced
 * nothing, a finished one did.
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
 * How much of what a program knows about it may touch. The remainder is not one thing: an asset
 * is outside the perimeter because a rule excludes it, or because no rule settles it and it is
 * kept and never probed. The bar draws the gap and names neither half, hence the row spells both
 * numbers out beside it.
 */
export function coverage(program: Program): number {
	const known = program.assets ?? 0;
	const inScope = program.assets_in_scope ?? 0;
	if (known <= 0) return 0;
	return Math.min(100, Math.max(0, Math.round((inScope / known) * 100)));
}
