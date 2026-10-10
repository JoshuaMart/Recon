<script lang="ts">
	import AssetRow from '$lib/components/AssetRow.svelte';
	import Facets from '$lib/components/Facets.svelte';
	import FilterBar from '$lib/components/FilterBar.svelte';
	import HostGroup from '$lib/components/HostGroup.svelte';
	import NameSearch from '$lib/components/NameSearch.svelte';
	import { groupHref, moreHref, nextHref } from '$lib/query';
	import type { Asset, FlatPage, Group, GroupedPage } from '$lib/types';
	import type { PageData } from './$types';

	const { data }: { data: PageData } = $props();

	/**
	 * The walk through the list, past the server's first page. The button used to link to the next page,
	 * replacing the fifty rows and sending the reader to the top; rows accumulate here instead. A
	 * derived, because that resets it: the server hands back a new `data` for every question, so a new
	 * answer empties this by construction. Filters stay in the URL and only depth is local.
	 */
	let walk = $derived({
		answer: data,
		groups: [] as Group[],
		assets: [] as Asset[],
		favicons: {} as Record<string, string>,
		/** Where the walk is. Empty means the list is complete. */
		cursor: data.nextCursor ?? '',
		failed: false
	});
	let loading = $state(false);

	const groups = $derived([...data.groups, ...walk.groups]);
	const assets = $derived([...data.assets, ...walk.assets]);
	// The page's map and the appended ones. A hash in both carries the same
	// image, because it is the hash of those bytes.
	const favicons = $derived({ ...data.favicons, ...walk.favicons });

	/**
	 * The no-JavaScript fallback link. A cursor, not an offset, and opaque on purpose: it encodes the
	 * ordering key the server chose. No page number or total, as a count over a filtered set is a
	 * second full scan, and that budget goes to the facets.
	 */
	const next = $derived(walk.cursor ? nextHref(data.filters, walk.cursor, data.grouped) : '');

	const shown = $derived(data.grouped ? groups.length : assets.length);

	async function more(event: MouseEvent) {
		event.preventDefault();
		if (loading || !walk.cursor) return;
		loading = true;
		try {
			const response = await fetch(moreHref(data.filters, walk.cursor, data.grouped), {
				headers: { accept: 'application/json' }
			});
			if (!response.ok) throw new Error('the console answered ' + response.status);
			const page: GroupedPage & FlatPage = await response.json();
			// What is on screen already: the walk is a keyset over a moving inventory, so a row whose last_seen
			// went backwards sorts below a cursor it was handed under and comes back. The list is keyed by host
			// and asset, so a repeat is a duplicate key, crashing the whole page, not one wrong row.
			const shownHosts = new Set(groups.map((group) => group.host));
			const shownAssets = new Set(assets.map((asset) => asset.asset_id));
			walk = {
				...walk,
				groups: [...walk.groups, ...(page.groups ?? []).filter((group) => !shownHosts.has(group.host))],
				assets: [...walk.assets, ...(page.assets ?? []).filter((asset) => !shownAssets.has(asset.asset_id))],
				favicons: { ...walk.favicons, ...(page.favicons ?? {}) },
				// The server hands out the next one or nothing, and nothing is the
				// end of the walk rather than an error.
				cursor: page.next_cursor ?? '',
				failed: false
			};
		} catch {
			// Said rather than swallowed, and the cursor is kept: what failed is
			// one request, so the same click is worth making again.
			walk = { ...walk, failed: true };
		} finally {
			loading = false;
		}
	}
</script>

<svelte:head><title>Search · recon</title></svelte:head>

<div class="work">
	<aside class="side">
		<!-- The one filter that is typed, not chosen, and it sits with the others: it produces the same `f`
		as a facet click, and the toolbar chip is how it is undone. -->
		<NameSearch filters={data.filters} grouped={data.grouped} />

		<Facets facets={data.facets} filters={data.filters} {favicons} operators={data.operators} grouped={data.grouped} />
	</aside>

	<main class="results">
		<!-- The names, so a chip reads `program is jomar.ovh` and not an unverifiable identifier. The load
		function resolves them only when a programme filter is present, keeping other searches free. -->
		<FilterBar filters={data.filters} programNames={data.programNames} grouped={data.grouped} />

		<!-- The fold: the host is the list and the flat one the exception, so the toggle names both shapes
		and neither is a stateful mode. Links, because the shape lives in the URL: shareable, and the back
		button means something. -->
		<div class="mode">
			<a class="toggle" class:on={data.grouped} href={groupHref(data.filters, true)}>by host</a>
			<a class="toggle" class:on={!data.grouped} href={groupHref(data.filters, false)}>every asset</a>
		</div>

		{#if data.grouped}
			{#each groups as group (group.host)}
				<HostGroup {group} filters={data.filters} enriched={data.enriched} {favicons} />
			{/each}
		{:else if assets.length}
			<!-- One panel, not a card each, with the grouped list's row component and the host written back
			in: a second row would be a second place for what a line has to say. -->
			<section class="flat">
				{#each assets as asset (asset.asset_id)}
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
				<!-- An anchor, not a button: without JavaScript it is still the next page, and the handler turns
				the same click into rows added under those already read. -->
				<a class="btn" class:busy={loading} href={next} onclick={more} aria-busy={loading}>
					{loading ? 'Loading…' : 'Load more'}
				</a>
				{#if walk.failed}
					<span class="failed">That page did not come back. Clicking again asks for the same one.</span>
				{/if}
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

	/* The sidebar is the search and the facets, which is why the border and the
	   column live here rather than on the facet list. */
	.side {
		border-right: 1px solid var(--border);
		min-width: 0;
		padding: 16px 14px 40px 18px;
	}

	.results {
		padding: 14px 18px 60px;
		min-width: 0;
	}

	/* Two links, not a segmented control: the shape is a URL, not component state. Sized down, a
	preference, with the rows under it being what someone came to read. */
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
		flex-direction: column;
		align-items: center;
		gap: 6px;
		padding-top: 10px;
	}

	.btn.busy {
		color: var(--ink-3);
	}

	.failed {
		font-size: 11.5px;
		color: var(--code-5xx);
	}
</style>
