<script lang="ts">
	import Icon from './Icon.svelte';
	import { encodeFilter, exportHref, href, label, withoutFilter, type Filter } from '$lib/query';

	interface Props {
		filters: Filter[];
		/** Programme names, keyed by identifier. Empty unless a filter carries one. */
		programNames?: Record<string, string>;
		/**
		 * Which shape the list is in, so a link out of it does not silently change
		 * it.
		 *
		 * `href` defaults to grouped, which is right for a link arriving from
		 * somewhere else and wrong for every link on this page: from the flat list,
		 * clicking a facet or removing a chip folded the list back without anybody
		 * asking. The shape lives in the URL, so it has to travel with every link
		 * built from it.
		 */
		grouped?: boolean;
	}

	const { filters, programNames = {}, grouped = true }: Props = $props();

	/**
	 * The export carries the current filters, and it is the same query as the list
	 * by construction rather than by discipline: there is no "export
	 * query", so the link comes from the same array the cards were rendered from.
	 */
	const exports = [
		{
			format: 'jsonl' as const,
			label: 'Full data',
			meta: 'JSONL',
			description: 'Every field, one asset per line'
		},
		{
			format: 'csv' as const,
			label: 'Spreadsheet',
			meta: 'CSV',
			description: 'Flattened columns'
		},
		{
			format: 'urls' as const,
			label: 'URLs only',
			meta: 'TXT',
			description: 'Openable web addresses, one per line'
		}
	];
</script>

<div class="toolbar">
	{#each filters as filter (encodeFilter(filter))}
		<span class="chip">
			<code>{label(filter, programNames)}</code>
			<a class="x" href={href(withoutFilter(filters, filter), grouped)} aria-label="Remove this filter">×</a>
		</span>
	{/each}

	{#if filters.length}
		<a class="link" href={href([], grouped)}>Clear all</a>
	{:else}
		<span class="hint">Everything in the inventory. Click a facet or a badge to narrow it.</span>
	{/if}

	<span class="spacer"></span>

	<details class="export-menu">
		<summary class="btn" aria-label="Choose an export format">
			<Icon name="download" />
			Export
			<span class="chevron" aria-hidden="true">⌄</span>
		</summary>
		<ul class="export-options" aria-label="Export formats">
			{#each exports as option (option.format)}
				<li>
					<a href={exportHref(filters, option.format)} data-sveltekit-reload>
						<span class="option-line">
							<strong>{option.label}</strong>
							<code>{option.meta}</code>
						</span>
						<small>{option.description}</small>
					</a>
				</li>
			{/each}
		</ul>
	</details>
</div>

<style>
	.toolbar {
		display: flex;
		align-items: center;
		gap: 8px;
		flex-wrap: wrap;
		margin-bottom: 12px;
	}

	.chip {
		display: inline-flex;
		align-items: center;
		gap: 6px;
		background: var(--card);
		border: 1px solid var(--border);
		border-radius: var(--radius-control);
		padding: 3px 4px 3px 8px;
		font-size: 12px;
		box-shadow: var(--card-shadow);
	}

	.chip code {
		font-family: var(--font-mono);
		font-size: 11.5px;
		color: var(--ink-2);
	}

	.chip .x {
		color: var(--ink-3);
		padding: 0 3px;
		line-height: 1;
		font-size: 13px;
		text-decoration: none;
	}

	.chip .x:hover {
		color: var(--code-5xx);
	}

	.export-menu {
		position: relative;
	}

	.export-menu summary {
		list-style: none;
	}

	.export-menu summary::-webkit-details-marker {
		display: none;
	}

	.export-menu[open] summary {
		border-color: var(--ink-3);
	}

	.export-menu summary:focus-visible {
		outline: 2px solid var(--signal);
		outline-offset: 2px;
	}

	.chevron {
		color: var(--ink-3);
		font-size: 13px;
		line-height: 1;
		margin-left: 2px;
	}

	.export-menu[open] .chevron {
		transform: rotate(180deg);
	}

	.export-options {
		position: absolute;
		top: calc(100% + 5px);
		right: 0;
		z-index: 10;
		width: min(238px, calc(100vw - 36px));
		margin: 0;
		padding: 4px;
		list-style: none;
		background: var(--card);
		border: 1px solid var(--border);
		border-radius: var(--radius-control);
		box-shadow: var(--card-shadow);
	}

	.export-options a {
		display: block;
		padding: 7px 8px;
		border-radius: var(--radius-control);
		text-decoration: none;
	}

	.export-options a:hover,
	.export-options a:focus-visible {
		background: var(--signal-bg);
		outline: none;
	}

	.option-line {
		display: flex;
		align-items: baseline;
		justify-content: space-between;
		gap: 12px;
	}

	.option-line strong {
		font-size: 12px;
		font-weight: 500;
	}

	.option-line code {
		font-family: var(--font-mono);
		font-size: 10px;
		color: var(--ink-3);
	}

	.export-options small {
		display: block;
		margin-top: 1px;
		font-size: 10.5px;
		color: var(--ink-3);
	}

	.hint {
		color: var(--ink-3);
		font-size: 12px;
	}
</style>
