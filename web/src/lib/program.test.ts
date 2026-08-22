import { describe, expect, it } from 'vitest';
import { authorisation, coverage, runStatus } from './program';
import type { Program, Run } from './types';

const now = Date.parse('2026-08-22T12:00:00Z');

const run = (state: string, startedAt?: string) => ({ state, started_at: startedAt }) as unknown as Run;

const program = (assets?: number, inScope?: number) => ({ assets, assets_in_scope: inScope }) as unknown as Program;

describe('authorisation', () => {
	it('reads an absent end date as a permission with no end, not as a missing field', () => {
		expect(authorisation(undefined, now)).toEqual({ label: 'no end date', tone: 'plain' });
		// A date the server could not have written still must not read as expired.
		expect(authorisation('not a date', now)).toEqual({ label: 'no end date', tone: 'plain' });
	});

	it('says a permission has run out rather than letting the program look active', () => {
		expect(authorisation('2026-08-19T12:00:00Z', now)).toMatchObject({ tone: 'expired' });
	});

	it('warns inside the last seven days, and counts the last hours as one day', () => {
		expect(authorisation('2026-08-26T12:00:00Z', now)).toEqual({ label: 'ends in 4 days', tone: 'soon' });
		// Ceil, and singular with it: "ends in 1 days" was the first thing this said.
		expect(authorisation('2026-08-22T20:00:00Z', now)).toEqual({ label: 'ends in 1 day', tone: 'soon' });
	});

	it('states the date beyond the window', () => {
		expect(authorisation('2026-12-31T23:59:59Z', now)).toEqual({ label: 'until 2026-12-31', tone: 'plain' });
	});
});

describe('runStatus', () => {
	it('separates a scan somebody is running from a run nothing picked up', () => {
		expect(runStatus(run('pending'))).toMatchObject({ label: 'Waiting for a scanner', stalled: true });
		expect(runStatus(run('running', '2026-08-22T11:00:00Z'))).toMatchObject({ label: 'Scanning', stalled: false });
	});

	it('holds both open states in flight, since neither allows a second run', () => {
		expect(runStatus(run('pending'))?.inFlight).toBe(true);
		expect(runStatus(run('running', '2026-08-22T11:00:00Z'))?.inFlight).toBe(true);
		expect(runStatus(run('completed'))?.inFlight).toBe(false);
	});

	it('names an expired run rather than folding it into a finished one', () => {
		// The sweeper closed it, so it produced nothing. A finished run did.
		expect(runStatus(run('expired'))).toMatchObject({ label: 'Last run expired', tone: 'failed' });
		expect(runStatus(run('failed'))).toMatchObject({ tone: 'failed' });
		expect(runStatus(run('completed'))).toMatchObject({ label: 'Finished', tone: 'idle' });
	});

	it('answers nothing for a program that has never run', () => {
		expect(runStatus(null)).toBeNull();
		expect(runStatus(undefined)).toBeNull();
	});
});

describe('coverage', () => {
	it('draws nothing on a program that knows about nothing, rather than dividing by zero', () => {
		expect(coverage(program(0, 0))).toBe(0);
		expect(coverage(program(undefined, undefined))).toBe(0);
	});

	it('is the share of what is known that a rule lets through', () => {
		expect(coverage(program(1190, 812))).toBe(68);
		expect(coverage(program(4, 4))).toBe(100);
	});

	it('never draws a bar longer than its track', () => {
		// Counts read from two statements can disagree for the width of a write.
		expect(coverage(program(4, 9))).toBe(100);
	});
});
