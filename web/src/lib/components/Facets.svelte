<script lang="ts">
	import { shownFacets } from '$lib/format';
	import {
		encodeFilter,
		facetFilter,
		facetHref,
		fieldLabel,
		href,
		withFilter,
		withoutFilter,
		type Filter
	} from '$lib/query';
	import type { Facet, Term } from '$lib/types';

	interface Props {
		facets: Facet[];
		filters: Filter[];
		/** The page's favicon images, keyed by hash. */
		favicons: Record<string, string>;
		/** What each field accepts, from the server rather than guessed. */
		operators: Record<string, string[]>;
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

	const { facets, filters, favicons, operators, grouped = true }: Props = $props();

	const shown = $derived(shownFacets(facets));

	/**
	 * Three shapes, and the fold says why one was not enough.
	 *
	 * A facet is an aggregation over the filtered result, and
	 * nothing decided how it renders. A single shape is paid for at both extremes: seven
	 * ports took seven rows each with a bar comparing nothing anyone reads, and thirteen
	 * favicons took thirteen rows of a hash nobody recognises.
	 *
	 * Short values become chips, long labels keep the bar where the magnitudes are the
	 * point, and favicons are drawn as what they are.
	 */
	const asChips = new Set([
		'kind',
		'lifecycle',
		'http_state',
		'dns_state',
		'tcp_state',
		'status_code',
		'port',
		'country',
		'is_cdn',
		'scope_status'
	]);
	const asIcons = new Set(['favicon_hash']);

	/** How many terms a bar list shows before the rest are folded. */
	const cap = 6;
	let expanded = $state<Record<string, boolean>>({});

	/**
	 * The facets asked for on their own, keyed by field.
	 *
	 * The sidebar is capped at twenty values per field and says so with a `+`,
	 * which used to be the end of it: a technology carried by twelve assets is in
	 * the inventory, is filterable, and had nothing to click. Opening a cut facet
	 * asks the server for the same aggregation over the same filtered set,
	 * bounded higher, so the values below the cut become what every other value
	 * already is.
	 *
	 * Reset with the filters, since a facet counts the filtered result: the
	 * answer opened under one search says nothing about the next one.
	 */
	let opened = $derived({ answer: filters, fields: {} as Record<string, Facet> });
	/** The page's images, plus the ones that came with an opened facet. */
	let images = $derived({ answer: filters, map: { ...favicons } });
	let loading = $state('');
	let failed = $state('');

	function termsOf(facet: { field: string; terms: Term[] }): Term[] {
		return opened.fields[facet.field]?.terms ?? facet.terms;
	}

	function isCut(facet: { field: string; truncated: boolean }): boolean {
		const full = opened.fields[facet.field];
		return full ? (full.truncated ?? false) : facet.truncated;
	}

	function visible(field: string, terms: Term[]): Term[] {
		return expanded[field] ? terms : terms.slice(0, cap);
	}

	/**
	 * What the control under a facet does, which is one of three things.
	 *
	 * A cut facet opens on every value first, since the folded terms are not the
	 * question: what is missing is below them. After that, and on a facet that was
	 * never cut, it is the fold over what is already here.
	 */
	async function open(facet: { field: string; truncated: boolean }) {
		if (!isCut(facet) || opened.fields[facet.field]) {
			expanded[facet.field] = !expanded[facet.field];
			return;
		}
		if (loading) return;
		loading = facet.field;
		failed = '';
		try {
			const response = await fetch(facetHref(filters, facet.field), { headers: { accept: 'application/json' } });
			if (!response.ok) throw new Error('the console answered ' + response.status);
			const page: { facets: Facet[]; favicons?: Record<string, string> } = await response.json();
			const full = page.facets?.find((one) => one.field === facet.field);
			// A facet that comes back empty is not an answer worth replacing the
			// twenty already on screen with.
			if (!full?.terms?.length) throw new Error('the facet came back empty');
			opened = { ...opened, fields: { ...opened.fields, [facet.field]: full } };
			// The images of the values that were below the cut. Without them the
			// favicon grid draws blank squares, which reads as a broken interface
			// rather than as the answer.
			images = { ...images, map: { ...images.map, ...(page.favicons ?? {}) } };
			expanded[facet.field] = true;
		} catch {
			// Said rather than swallowed, and the click is worth making again.
			failed = facet.field;
		} finally {
			loading = '';
		}
	}

	/** The label says which of the three the next click is. */
	function control(facet: { field: string; terms: Term[]; truncated: boolean }): string {
		if (loading === facet.field) return 'loading…';
		if (failed === facet.field) return 'that did not come back, try again';
		if (isCut(facet) && !opened.fields[facet.field]) return 'every value';
		if (expanded[facet.field]) return 'show fewer';
		return termsOf(facet).length - cap + ' more';
	}

	/** Whether a facet has anything left to offer under it. */
	function hasControl(facet: { field: string; terms: Term[]; truncated: boolean }, folds: boolean): boolean {
		if (isCut(facet) && !opened.fields[facet.field]) return true;
		if (failed === facet.field || loading === facet.field) return true;
		return folds && termsOf(facet).length > cap;
	}

	function width(terms: Term[], count: number): string {
		const top = Math.max(...terms.map((term) => term.count), 1);
		return Math.max(3, Math.round((count / top) * 100)) + '%';
	}

	/** Whether this term is one of the filters in force, so the sidebar can say so. */
	function active(field: string, value: string): Filter | undefined {
		const wanted = encodeFilter(facetFilter(field, value, operators));
		return filters.find((filter) => encodeFilter(filter) === wanted);
	}

	/** A bucket already in force links to its own removal, which is where a reader clicks next. */
	function termHref(field: string, value: string): string {
		const on = active(field, value);
		if (on) return href(withoutFilter(filters, on), grouped);
		return href(withFilter(filters, facetFilter(field, value, operators)), grouped);
	}

	/** Labels for the values the database spells in its own vocabulary. */
	const valueLabels: Record<string, string> = { true: 'yes', false: 'no' };

	function label(value: string): string {
		return valueLabels[value] ?? value;
	}
</script>

<aside class="facets">
	<h2>Facets</h2>

	{#each shown as facet (facet.field)}
		<section class="facet">
			<h3>
				{fieldLabel(facet.field)}
				<!-- The count says the facet was cut, because a truncated list that looks
				     complete is a statement about the inventory and it is false. -->
				<span class="n" class:cut={isCut(facet)} title={isCut(facet) ? 'more values than this list shows' : ''}
					>{termsOf(facet).length}{isCut(facet) ? '+' : ''}</span
				>
			</h3>

			{#if asIcons.has(facet.field)}
				<!-- A favicon is an image, and it is the fastest identity signal an inventory
				     has. Thirteen hashes were thirteen unreadable rows; the hash
				     goes to the hover, where it is still the value the filter carries. -->
				<div class="icons">
					{#each termsOf(facet) as bucket (bucket.value)}
						<a
							class="ico"
							class:on={active(facet.field, bucket.value)}
							href={termHref(facet.field, bucket.value)}
							title="{bucket.value} · {bucket.count} assets"
						>
							{#if images.map[bucket.value]}
								<img src={images.map[bucket.value]} alt="" />
							{:else}
								<span class="unknown" aria-hidden="true"></span>
							{/if}
							<span class="c">{bucket.count}</span>
						</a>
					{/each}
				</div>
				{@render more(facet, false)}
			{:else if asChips.has(facet.field)}
				<div class="chips">
					{#each termsOf(facet) as bucket (bucket.value)}
						<a class="chip" class:on={active(facet.field, bucket.value)} href={termHref(facet.field, bucket.value)}>
							<span class="k">{label(bucket.value)}</span>
							<span class="c">{bucket.count}</span>
						</a>
					{/each}
				</div>
				<!-- A chip list is short by nature and folds at nothing, so its control
				     only ever appears to open a facet the cap cut. -->
				{@render more(facet, false)}
			{:else}
				<ul class="bars">
					{#each visible(facet.field, termsOf(facet)) as bucket (bucket.value)}
						<li>
							<a class="bucket" class:on={active(facet.field, bucket.value)} href={termHref(facet.field, bucket.value)}>
								<span class="text">{label(bucket.value)}</span>
								<span class="bar"><i style:width={width(termsOf(facet), bucket.count)}></i></span>
								<span class="c">{bucket.count}</span>
							</a>
						</li>
					{/each}
				</ul>
				{@render more(facet, true)}
			{/if}
		</section>
	{/each}
</aside>

<!--
	One control for the three shapes.

	`folds` says whether the list under it is capped on this side, which only the
	bars are. Everything else it does is the same everywhere: a facet the server
	cut opens on every value, and what is already here folds.
-->
{#snippet more(facet: { field: string; terms: Term[]; truncated: boolean }, folds: boolean)}
	{#if hasControl(facet, folds)}
		<button class="more" class:failed={failed === facet.field} type="button" onclick={() => open(facet)}>
			{control(facet)}
		</button>
	{/if}
{/snippet}

<style>
	.n.cut {
		color: var(--code-4xx);
	}

	/* The column, its padding and its border belong to the sidebar this sits in:
	   the search field above shares them. */
	.facets {
		min-width: 0;
	}

	h2 {
		font-size: 11px;
		text-transform: uppercase;
		letter-spacing: 0.07em;
		color: var(--ink-3);
		font-weight: 600;
		margin: 0 0 12px;
	}

	.facet {
		margin-bottom: 16px;
	}

	h3 {
		font-size: 10.5px;
		text-transform: uppercase;
		letter-spacing: 0.07em;
		color: var(--ink-3);
		font-weight: 600;
		margin: 0 0 7px;
		display: flex;
		align-items: baseline;
		gap: 6px;
	}

	h3 .n {
		color: var(--border);
		font-weight: 500;
	}

	.chips {
		display: flex;
		flex-wrap: wrap;
		gap: 4px;
	}

	.chip {
		display: inline-flex;
		align-items: center;
		gap: 5px;
		border: 1px solid var(--border);
		background: var(--card);
		border-radius: var(--radius-control);
		padding: 2px 6px;
		font-size: 11.5px;
		color: var(--ink-2);
		text-decoration: none;
		max-width: 100%;
	}

	.chip .k {
		font-family: var(--font-mono);
		overflow: hidden;
		text-overflow: ellipsis;
	}

	.chip .c {
		font-family: var(--font-mono);
		font-size: 10.5px;
		color: var(--ink-3);
	}

	.chip:hover {
		border-color: var(--ink-3);
	}

	/* A value already in force is shown as such and links to its own removal: the
	   sidebar used to leave a chosen bucket looking exactly like an unchosen one. */
	.chip.on {
		background: var(--signal-bg);
		border-color: #bce8d8;
		color: #0a7a58;
	}

	.chip.on .c {
		color: #0a7a58;
	}

	.bars {
		list-style: none;
		margin: 0;
		padding: 0;
	}

	.bucket {
		display: grid;
		grid-template-columns: 1fr 34px 30px;
		align-items: center;
		gap: 6px;
		padding: 3px 4px;
		border-radius: var(--radius-control);
		text-decoration: none;
	}

	.bucket:hover {
		background: var(--signal-bg);
	}

	.bucket.on {
		background: var(--signal-bg);
	}

	.bucket.on .text {
		color: #0a7a58;
		font-weight: 500;
	}

	.text {
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
		color: var(--ink-2);
		font-size: 12px;
	}

	.bar {
		height: 3px;
		background: var(--border);
		border-radius: 2px;
		overflow: hidden;
	}

	.bar i {
		display: block;
		height: 100%;
		background: var(--ink-3);
	}

	.bucket .c {
		font-family: var(--font-mono);
		font-size: 11px;
		color: var(--ink-3);
		text-align: right;
	}

	.more {
		background: none;
		border: 0;
		padding: 3px 4px;
		font-size: 11px;
		color: var(--ink-3);
		text-decoration: underline;
		text-decoration-color: var(--border);
	}

	.more:hover {
		color: var(--ink);
	}

	.more.failed {
		color: var(--code-5xx);
	}

	.icons {
		display: grid;
		grid-template-columns: repeat(auto-fill, minmax(28px, 1fr));
		gap: 7px 6px;
	}

	.ico {
		position: relative;
		width: 26px;
		height: 26px;
		border-radius: 5px;
		border: 1px solid var(--border);
		background: var(--card);
		display: grid;
		place-items: center;
	}

	.ico img {
		width: 16px;
		height: 16px;
		image-rendering: pixelated;
	}

	.ico .unknown {
		width: 10px;
		height: 10px;
		border-radius: 2px;
		background: var(--border);
	}

	.ico:hover {
		border-color: var(--ink-3);
	}

	.ico.on {
		border-color: var(--signal);
		box-shadow: 0 0 0 2px var(--signal-bg);
	}

	.ico .c {
		position: absolute;
		right: -4px;
		bottom: -5px;
		background: var(--card);
		border: 1px solid var(--border);
		border-radius: 3px;
		font-family: var(--font-mono);
		font-size: 8.5px;
		font-weight: 600;
		color: var(--ink-3);
		padding: 0 2px;
	}
</style>
