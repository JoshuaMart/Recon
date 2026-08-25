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
	 * The walk through the list, past the first page the server rendered.
	 *
	 * The button used to be a link to the next page, which is a different thing
	 * from loading more: it replaced the fifty rows on screen with the next fifty
	 * and sent the reader back to the top, so a list read downwards could only be
	 * walked by never looking back. The rows accumulate here instead.
	 *
	 * A derived rather than plain state, because that is what resets it: the
	 * server hands back a new `data` for every question, so a new answer empties
	 * this by construction, and appending assigns over it until the next one.
	 * That is also the line this splits on. The filters stay in the URL and only
	 * the depth is local: a shared link is the search, not how far somebody read.
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
	 * The link the button falls back to with no JavaScript.
	 *
	 * A cursor and not an offset, and the cursor is opaque on purpose: it encodes
	 * the ordering key the server chose. There is no page number and no total,
	 * because a count over a filtered set is a second full scan, and that budget is spent
	 * that budget on the facets instead.
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
			// What is already on screen, because the walk is a keyset over an
			// inventory that keeps moving: a row whose last_seen went backwards
			// sorts below a cursor it was already handed out under, and comes
			// back. The list is keyed by host and by asset, so a repeat is a
			// duplicate key, which is a crash of the whole page rather than one
			// wrong row.
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
		<!-- The one filter that is typed rather than chosen, and it sits with the
		     others: it produces the same `f` in the URL as a facet click, and the
		     chip that appears in the toolbar is how it is undone. -->
		<NameSearch filters={data.filters} grouped={data.grouped} />

		<Facets facets={data.facets} filters={data.filters} {favicons} operators={data.operators} grouped={data.grouped} />
	</aside>

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
			{#each groups as group (group.host)}
				<HostGroup {group} filters={data.filters} enriched={data.enriched} {favicons} />
			{/each}
		{:else if assets.length}
			<!-- One panel rather than one card each, and the same row component as the
			     grouped list with the host written back into it. A second row would be a
			     second place to keep what a line has to say. -->
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
				<!-- An anchor and not a button: without JavaScript this is still the
				     next page, and the handler is what turns the same click into rows
				     added under the ones already read. -->
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
