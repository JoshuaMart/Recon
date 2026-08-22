import { describe, expect, it } from 'vitest';
import { safeNext } from './next';

describe('safeNext', () => {
	it('keeps a path, which is what the redirect is for', () => {
		expect(safeNext('/assets/3f9a?f=port%3Aeq%3A443')).toBe('/assets/3f9a?f=port%3Aeq%3A443');
		expect(safeNext('/')).toBe('/');
	});

	// The discriminating cases, and the reason the module exists: both start with a
	// slash and both leave the console. A guard on the first character passes them.
	it('refuses a protocol-relative address in both of its spellings', () => {
		expect(safeNext('//evil.example/login')).toBe('/');
		expect(safeNext('/\\evil.example/login')).toBe('/');
	});

	it('refuses an absolute address and anything that is not a path', () => {
		expect(safeNext('https://evil.example')).toBe('/');
		expect(safeNext('javascript:alert(1)')).toBe('/');
		expect(safeNext('')).toBe('/');
		expect(safeNext(null)).toBe('/');
	});
});
