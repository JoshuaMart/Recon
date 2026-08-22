import { describe, expect, it } from 'vitest';
import { claimed, depthsByProgram, lastDiscoveryRuns, queueLines } from './queue';
import type { Program, QueueDepth, Run } from './types';

const program = (id: string, name: string) => ({ id, name }) as Program;

const depth = (programID: string, queue: string, due: number, later = 0, inRun = 0) =>
	({ program_id: programID, queue, due, later, in_run: inRun }) as unknown as QueueDepth;

describe('queueLines', () => {
	it('renders every queue, including the ones the server had no row for', () => {
		const lines = queueLines([depth('p1', 'fingerprint', 4)], [program('p1', 'Jomar')]);

		expect(lines.map((line) => line.queue)).toEqual(['fingerprint', 'full', 'resolve']);
		// An empty queue is a zero somebody can read, not an absence.
		expect(lines.find((line) => line.queue === 'resolve')).toMatchObject({ due: 0, shares: [] });
	});

	it('sums the programs of one queue', () => {
		const lines = queueLines(
			[depth('p1', 'fingerprint', 4, 10, 1), depth('p2', 'fingerprint', 2, 0, 3)],
			[program('p1', 'Jomar'), program('p2', 'Other')]
		);

		expect(lines[0]).toMatchObject({ due: 6, later: 10, in_run: 4 });
		expect(lines[0].shares.map((share) => share.name)).toEqual(['Jomar', 'Other']);
	});

	it('keeps a depth whose program is not in the list, under its identifier', () => {
		const lines = queueLines([depth('99999999-aaaa', 'full', 7)], []);

		const full = lines.find((line) => line.queue === 'full');
		expect(full?.due).toBe(7);
		expect(full?.shares[0].name).toBe('99999999');
	});
});

describe('claimed', () => {
	it('separates a scan somebody is running from a run nothing picked up', () => {
		expect(claimed('pending', undefined)).toBe(false);
		expect(claimed('pending', '2026-08-19T16:40:05Z')).toBe(true);
		expect(claimed('running', '2026-08-19T16:40:05Z')).toBe(true);
		// A finished run is claimed whatever it carries: it ran.
		expect(claimed('completed', undefined)).toBe(true);
	});
});

describe('depthsByProgram', () => {
	it('adds the three schedules of one program into the one number a row shows', () => {
		const totals = depthsByProgram([
			depth('p1', 'fingerprint', 4, 10, 1),
			depth('p1', 'full', 2, 5, 0),
			depth('p1', 'resolve', 0, 1, 3),
			depth('p2', 'full', 7, 0, 0)
		]);

		expect(totals.get('p1')).toEqual({ due: 6, later: 16, in_run: 4 });
		expect(totals.get('p2')).toEqual({ due: 7, later: 0, in_run: 0 });
	});

	it('answers nothing for a program with no row, which the row draws as an absence', () => {
		expect(depthsByProgram([]).get('p1')).toBeUndefined();
	});
});

describe('lastDiscoveryRuns', () => {
	const run = (programID: string, kind: string, id: string) => ({ id, program_id: programID, kind }) as unknown as Run;

	it('keeps the first row per program, since the queue answers newest first', () => {
		const last = lastDiscoveryRuns([
			run('p1', 'discovery', 'newest'),
			run('p1', 'discovery', 'older'),
			run('p2', 'discovery', 'other')
		]);

		expect(last.get('p1')?.id).toBe('newest');
		expect(last.get('p2')?.id).toBe('other');
	});

	it('ignores verification, which holds the same program and answers another question', () => {
		// A row saying "scanning" because something is verifying would send
		// somebody looking for a perimeter walk that is not happening.
		const last = lastDiscoveryRuns([run('p1', 'verification', 'verify'), run('p1', 'discovery', 'walk')]);

		expect(last.get('p1')?.id).toBe('walk');
	});
});
