import { describe, expect, it } from 'vitest';
import { render } from 'svelte/server';
import FilterBar from './FilterBar.svelte';

const body = () =>
	render(FilterBar, {
		props: { filters: [{ field: 'kind', op: 'eq' as const, value: 'service' }] }
	}).body;

describe('FilterBar export menu', () => {
	it('offers full, spreadsheet and URL-only exports for the current filters', () => {
		const html = body();
		expect(html).toContain('Full data');
		expect(html).toContain('Spreadsheet');
		expect(html).toContain('URLs only');
		expect(html).toContain('format=jsonl');
		expect(html).toContain('format=csv');
		expect(html).toContain('format=urls');
		expect(html.match(/f=kind%3Aeq%3Aservice/g)).toHaveLength(3);
	});
});
