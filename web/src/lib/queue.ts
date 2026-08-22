import type { Program, QueueDepth } from './types';

/**
 * The three queues, in the order somebody reads them.
 *
 * They are the due date columns and not the observation layers, and the
 * difference is worth keeping: there are three schedules and four layers, so
 * naming them after the layers would promise a tcp queue that does not exist.
 * `fingerprint` first because it is the one people come to this page for: its
 * unit costs two orders of magnitude more than a probe, and its depth translates
 * directly into minutes of browser.
 *
 * The list is fixed rather than derived from the answer. A queue the server has
 * no row for is a queue at zero, and rendering nothing for it would leave the
 * reader unable to tell "empty" from "this schedule does not exist here".
 */
export const QUEUES = ['fingerprint', 'full', 'resolve'] as const;

export interface QueueShare {
	program_id: string;
	name: string;
	due: number;
	later: number;
	in_run: number;
}

export interface QueueLine {
	queue: string;
	due: number;
	later: number;
	in_run: number;
	/** The programs holding any of it, largest queue first. */
	shares: QueueShare[];
}

/**
 * queueLines folds the per-program depths into one line per queue.
 *
 * The names come from the program list the layout already loads, so this page
 * asks the control plane for nothing extra. A depth whose program is not in that
 * list still shows, under its identifier: dropping it would hide work that
 * exists, which is the one thing a queue view must not do.
 */
export function queueLines(depths: QueueDepth[], programs: Program[]): QueueLine[] {
	const names = new Map(programs.map((program) => [program.id, program.name]));

	return QUEUES.map((queue) => {
		const shares = depths
			.filter((depth) => depth.queue === queue)
			.map((depth) => ({
				program_id: depth.program_id,
				name: names.get(depth.program_id) ?? depth.program_id.slice(0, 8),
				due: depth.due,
				later: depth.later,
				in_run: depth.in_run
			}))
			.sort((a, b) => b.due - a.due || b.later - a.later || a.name.localeCompare(b.name));

		return {
			queue,
			due: sum(shares, 'due'),
			later: sum(shares, 'later'),
			in_run: sum(shares, 'in_run'),
			shares
		};
	});
}

function sum(shares: QueueShare[], field: 'due' | 'later' | 'in_run'): number {
	return shares.reduce((total, share) => total + share[field], 0);
}

/**
 * What a run's state means when nothing has claimed it.
 *
 * The whole distinction turns on this: a run with no `started_at` is not a scan
 * to wait for, it is a provisioning that never reached a scanner. The two want
 * opposite reactions, so the screen has to separate them.
 */
export function claimed(state: string, startedAt: string | undefined): boolean {
	return state !== 'pending' || Boolean(startedAt);
}
