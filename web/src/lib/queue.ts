import type { Program, QueueDepth, Run } from './types';

/**
 * The three queues, in reading order. They are the due-date columns, not the observation layers
 * (three schedules against four layers; layer names would promise a tcp queue that does not exist).
 * `fingerprint` first: its unit costs two orders of magnitude more than a probe. The list is fixed,
 * not derived, so a queue the server has no row for reads zero and "empty" is not confused with
 * "this schedule does not exist here".
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
 * queueLines folds the per-program depths into one line per queue. Names come from the program
 * list the layout already loads, so this page asks the control plane for nothing extra. A depth
 * whose program is not listed still shows under its identifier: dropping it would hide work that
 * exists, which a queue view must not do.
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
 * What a run's state means when nothing has claimed it. A run with no `started_at` is not a scan
 * to wait for but a provisioning that never reached a scanner; the two want opposite reactions,
 * so the screen must separate them.
 */
export function claimed(state: string, startedAt: string | undefined): boolean {
	return state !== 'pending' || Boolean(startedAt);
}

export interface QueueTotals {
	due: number;
	later: number;
	in_run: number;
}

/**
 * The three queues of one program, added up. queueLines folds the other way (a line per queue
 * across programs, the queue page's question); the program list asks whether anything is waiting
 * on this perimeter, whichever schedule. Adding them makes one number, and the panels showing it
 * say so, so it is not read as a single queue.
 */
export function depthsByProgram(depths: QueueDepth[]): Map<string, QueueTotals> {
	const out = new Map<string, QueueTotals>();

	for (const depth of depths) {
		const total = out.get(depth.program_id) ?? { due: 0, later: 0, in_run: 0 };
		total.due += depth.due;
		total.later += depth.later;
		total.in_run += depth.in_run;
		out.set(depth.program_id, total);
	}

	return out;
}

/**
 * The last discovery run of each program. The queue answers newest first, so a program's first row is
 * its last run; one pass, not a find() per row. Discovery only: a verification run holds the same
 * program and answers another question, and "scanning" because something is verifying would send
 * someone looking for a perimeter walk that is not happening.
 */
export function lastDiscoveryRuns(runs: Run[]): Map<string, Run> {
	const out = new Map<string, Run>();

	for (const run of runs) {
		if (run.kind !== 'discovery') continue;
		if (!out.has(run.program_id)) out.set(run.program_id, run);
	}

	return out;
}
