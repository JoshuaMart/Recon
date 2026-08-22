<script lang="ts">
	import AssetRow from '$lib/components/AssetRow.svelte';
	import Facets from '$lib/components/Facets.svelte';
	import FilterBar from '$lib/components/FilterBar.svelte';
	import HostGroup from '$lib/components/HostGroup.svelte';
	import { groupHref, nextHref } from '$lib/query';
	import type { PageData } from './$types';

	const { data }: { data: PageData } = $props();

	/**
	 * The link to the next page.
	 *
	 * A cursor and not an offset, and the cursor is opaque on purpose: it encodes
	 * the ordering key the server chose. There is no page number and no total,
	 * because a count over a filtered set is a second full scan, and that budget is spent
	 * that budget on the facets instead.
	 */
	const next = $derived(data.nextCursor ? nextHref(data.filters, data.nextCursor, data.grouped) : '');

	const shown = $derived(data.grouped ? data.groups.length : data.assets.length);
</script>

<svelte:head><title>Search · recon</title></svelte:head>

<div class="work">
	<Facets
		facets={data.facets}
		filters={data.filters}
		favicons={data.favicons}
		operators={data.operators}
		grouped={data.grouped}
	/>

	<main class="results">
		<!-- The names, so a chip reads `program is jomar.ovh` rather than an identifier
		     nobody can check. The load function resolves them only when a programme filter
		     is present, which is what keeps this free on every other search. -->
		<FilterBar filters={data.filters} programNames={data.programNames} grouped={data.grouped} />

		<!-- the fold: the host is the list and the flat one is the exception, so the
		     toggle names both shapes and neither is a mode with state. Links rather than
		     a control, because the shape lives in the URL: it is shareable and the back
		     button means something. -->
		<div class="mode">
			<a class="toggle" class:on={data.grouped} href={groupHref(data.filters, true)}>by host</a>
			<a class="toggle" class:on={!data.grouped} href={groupHref(data.filters, false)}>every asset</a>
		</div>

		{#if data.grouped}
			{#each data.groups as group (group.host)}
				<HostGroup {group} filters={data.filters} enriched={data.enriched} favicons={data.favicons} />
			{/each}
		{:else if data.assets.length}
			<!-- One panel rather than one card each, and the same row component as the
			     grouped list with the host written back into it. A second row would be a
			     second place to keep what a line has to say. -->
			<section class="flat">
				{#each data.assets as asset (asset.asset_id)}
					<AssetRow {asset} filters={data.filters} withHost grouped={false} />
				{/each}
			</section>
		{/if}

		{#if shown === 0}
			<p class="empty">
				Nothing matches these filters. The inventory keeps assets it has decided are out of scope, so an empty result is
				a statement about the filters and not about the perimeter.
			</p>
		{/if}

		{#if next}
			<div class="next">
				<a class="btn" href={next}>Load more</a>
			</div>
		{/if}
	</main>
</div>

<style>
	.work {
		display: grid;
		grid-template-columns: 232px 1fr;
		flex: 1;
		min-height: 0;
	}

	.results {
		padding: 14px 18px 60px;
		min-width: 0;
	}

	/* Two links rather than a segmented control, because the shape is a URL and not a
	   piece of component state. Sized down: it is a preference, and the rows under it
	   are what somebody came to read. */
	.mode {
		display: flex;
		gap: 2px;
		margin: -4px 0 10px;
	}

	.toggle {
		font-size: 11.5px;
		color: var(--ink-3);
		text-decoration: none;
		padding: 2px 8px;
		border-radius: 999px;
	}

	.toggle:hover {
		color: var(--ink);
	}

	.toggle.on {
		background: var(--signal-bg);
		color: var(--ink);
		font-weight: 500;
	}

	.flat {
		background: var(--card);
		border: 1px solid var(--border);
		border-radius: var(--radius-card);
		box-shadow: var(--card-shadow);
		overflow: hidden;
	}

	.flat :global(.row:first-child) {
		border-top: 0;
	}

	.empty {
		background: var(--card);
		border: 1px dashed var(--border);
		border-radius: var(--radius-card);
		padding: 18px;
		color: var(--ink-3);
		font-size: 12.5px;
		margin: 0;
	}

	.next {
		display: flex;
		justify-content: center;
		padding-top: 10px;
	}
</style>
