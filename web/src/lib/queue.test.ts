import { describe, expect, it } from 'vitest';
import { claimed, queueLines } from './queue';
import type { Program, QueueDepth } from './types';

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
