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

describe('FilterBar alternatives', () => {
	it('shows same-facet values as OR and keeps each value removable', () => {
		const html = render(FilterBar, {
			props: {
				filters: [
					{ field: 'status_code', op: 'eq' as const, value: '200' },
					{ field: 'kind', op: 'eq' as const, value: 'service' },
					{ field: 'status_code', op: 'eq' as const, value: '302' }
				]
			}
		}).body;

		expect(html).toContain('status is 200');
		expect(html).toContain('status is 302');
		expect(html).toMatch(/class="or [^"]+">or<\/span>/);
		expect(html).toContain('f=kind%3Aeq%3Aservice&amp;f=status_code%3Aeq%3A302');
		expect(html).toContain('f=status_code%3Aeq%3A200&amp;f=kind%3Aeq%3Aservice');
	});
});
