<script lang="ts">
	import Icon from './Icon.svelte';
	import { encodeFilter, exportHref, href, label, withoutFilter, type Filter } from '$lib/query';

	interface Props {
		filters: Filter[];
		/** Programme names, keyed by identifier. Empty unless a filter carries one. */
		programNames?: Record<string, string>;
	}

	const { filters, programNames = {} }: Props = $props();

	/**
	 * The export carries the current filters, and it is the same query as the list
	 * by construction rather than by discipline: there is no "export
	 * query", so the link comes from the same array the cards were rendered from.
	 */
	const download = $derived(exportHref(filters));
</script>

<div class="toolbar">
	{#each filters as filter (encodeFilter(filter))}
		<span class="chip">
			<code>{label(filter, programNames)}</code>
			<a class="x" href={href(withoutFilter(filters, filter))} aria-label="Remove this filter">×</a>
		</span>
	{/each}

	{#if filters.length}
		<a class="link" href="/">Clear all</a>
	{:else}
		<span class="hint">Everything in the inventory. Click a facet or a badge to narrow it.</span>
	{/if}

	<span class="spacer"></span>

	<a class="btn" href={download} data-sveltekit-reload>
		<Icon name="download" />
		Export
	</a>
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

	.hint {
		color: var(--ink-3);
		font-size: 12px;
	}
</style>
