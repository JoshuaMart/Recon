<script lang="ts">
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { onMount } from 'svelte';
	import { api, ApiError, qs } from '#lib/api.ts';
	import { ago, count, displayKey, label, splitTech } from '#lib/format.ts';
	import { terms, toggle, without } from '#lib/query.ts';
	import type { FacetValue, Page, Row, SavedSearch } from '#lib/types.ts';
	import Shot from '#lib/components/Shot.svelte';
	import Status from '#lib/components/Status.svelte';
	import Lifecycle from '#lib/components/Lifecycle.svelte';
	import More from '#lib/components/More.svelte';

	interface Field {
		name: string;
		kind: string;
		doc: string;
		example: string;
	}

	const facetOrder: [string, string][] = [
		['technologies', 'Technologies'],
		['status', 'Status'],
		['ports', 'Ports'],
		['programs', 'Programs'],
		['triage', 'Triage'],
		['state', 'Lifecycle'],
		['cdn', 'CDN']
	];

	const q = $derived(page.url.searchParams.get('q') ?? '');
	const view = $derived(page.url.searchParams.get('view') === 'hosts' ? 'hosts' : 'services');
	const layout = $derived(page.url.searchParams.get('layout') === 'grid' ? 'grid' : 'list');

	let draft = $state('');
	let rows = $state<Row[]>([]);
	let next = $state<string | undefined>();
	let facets = $state<Record<string, FacetValue[]>>({});
	let error = $state<ApiError | null>(null);
	let loading = $state(false);
	let more = $state(false);
	let saved = $state<SavedSearch[]>([]);
	let fields = $state<Field[]>([]);
	let saving = $state(false);
	let saveName = $state('');
	let saveError = $state('');
	let input: HTMLInputElement;
	let ticket = 0;

	function nav(params: { q?: string; view?: string; layout?: string }) {
		const p = { q, view, layout, ...params };
		goto('/' + qs({ q: p.q, view: p.view === 'services' ? '' : p.view, layout: p.layout === 'list' ? '' : p.layout }), {
			reset: false
		});
	}

	$effect(() => {
		draft = q;
		const t = ++ticket;
		loading = true;
		error = null;
		const query = qs({ q, view });
		Promise.all([api.get<Page>('/api/search' + query), api.get<{ facets: Record<string, FacetValue[]> }>('/api/search/facets' + query)])
			.then(([res, f]) => {
				if (t !== ticket) return;
				rows = res.rows;
				next = res.next;
				facets = f.facets;
			})
			.catch((e) => {
				if (t !== ticket) return;
				error = e instanceof ApiError ? e : new ApiError(0, { detail: String(e) });
				rows = [];
				next = undefined;
				facets = {};
			})
			.finally(() => {
				if (t === ticket) loading = false;
			});
	});

	async function loadMore() {
		if (!next || more) return;
		more = true;
		const t = ticket;
		try {
			const res = await api.get<Page>('/api/search' + qs({ q, view, cursor: next }));
			// A page of the previous query is dropped.
			if (t !== ticket) return;
			rows = [...rows, ...res.rows];
			next = res.next;
		} finally {
			more = false;
		}
	}

	async function loadSaved() {
		saved = (await api.get<{ saved_searches: SavedSearch[] }>('/api/saved-searches')).saved_searches;
	}

	onMount(() => {
		loadSaved();
		api.get<{ fields: Field[] }>('/api/search/fields').then((r) => (fields = r.fields));
		const onKey = (e: KeyboardEvent) => {
			const t = e.target as HTMLElement;
			if (e.key === '/' && !['INPUT', 'TEXTAREA', 'SELECT'].includes(t.tagName)) {
				e.preventDefault();
				input?.focus();
				input?.select();
			}
		};
		window.addEventListener('keydown', onKey);
		return () => window.removeEventListener('keydown', onKey);
	});

	function submit(e: SubmitEvent) {
		e.preventDefault();
		nav({ q: draft.trim() });
	}

	async function save(e: SubmitEvent) {
		e.preventDefault();
		saveError = '';
		try {
			await api.post('/api/saved-searches', { name: saveName, query: q, view });
			saving = false;
			saveName = '';
			await loadSaved();
		} catch (err) {
			saveError = err instanceof ApiError ? err.message : String(err);
		}
	}

	const chips = $derived(terms(q));
	const active = $derived(new Set(chips));
	const exportURL = (format: string) => '/api/search/export' + qs({ q, view, format });
	const savedMatch = $derived(saved.find((s) => s.query === q && s.view === view));
	const errSuggest = $derived(error?.body.suggest && error.body.field ? q.replaceAll(error.body.field + ':', error.body.suggest + ':') : '');

	const key = (r: Row) => displayKey(r.kind, r.key);
</script>

<svelte:head><title>{q ? q + ' · ' : ''}Search · Recon</title></svelte:head>

<div class="page search">
	<section aria-label="Query" class="query-area">
		<div class="query-row">
			<form class="query" onsubmit={submit} role="search">
				<label for="q" class="sr-only">Query</label>
				<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true"
					><circle cx="11" cy="11" r="7" /><path d="m20 20-3.5-3.5" stroke-linecap="round" /></svg
				>
				<input
					id="q"
					bind:this={input}
					bind:value={draft}
					spellcheck="false"
					autocomplete="off"
					autocapitalize="off"
					placeholder="tech:nginx@<1.25 status:2xx -cdn:*"
				/>
				<kbd aria-hidden="true">/</kbd>
			</form>
			<div class="actions">
				{#if saved.length}
					<label class="sr-only" for="saved">Saved searches</label>
					<select
						id="saved"
						class="select"
						value={savedMatch?.id ?? ''}
						onchange={(e) => {
							const s = saved.find((x) => x.id === e.currentTarget.value);
							if (s) nav({ q: s.query, view: s.view });
						}}
					>
						<option value="">Saved searches</option>
						{#each saved as s (s.id)}<option value={s.id}>{s.name}</option>{/each}
					</select>
				{/if}
				<button class="btn" type="button" onclick={() => (saving = !saving)} disabled={!!savedMatch} aria-expanded={saving}>
					{savedMatch ? 'Saved' : 'Save search'}
				</button>
				<details class="menu">
					<summary class="btn">Export</summary>
					<div class="menu-list">
						<a href={exportURL('urls')} download="urls.txt">URLs</a>
						<a href={exportURL('hosts')} download="hosts.txt">Hosts</a>
						<a href={exportURL('csv')} download="results.csv">CSV</a>
						<a href={exportURL('json')} download="results.ndjson">JSON lines</a>
					</div>
				</details>
			</div>
		</div>

		{#if saving}
			<form class="save" onsubmit={save}>
				<label for="save-name">Name</label>
				<input id="save-name" class="input" bind:value={saveName} required {@attach (el) => el.focus()} />
				<button class="btn btn-primary" type="submit" disabled={!saveName.trim()}>Save</button>
				<button class="btn" type="button" onclick={() => (saving = false)}>Cancel</button>
				{#if saveError}<span class="error">{saveError}</span>{/if}
			</form>
		{/if}

		<div class="query-meta">
			{#if chips.length}
				<ul aria-label="Terms" class="terms">
					{#each chips as t, i (t + i)}
						<li>
							<span>{t}</span>
							<button type="button" class="chip-x" aria-label="Remove {t}" onclick={() => nav({ q: without(q, i) })}>
								<svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" aria-hidden="true"
									><path d="M6 6l12 12M18 6 6 18" /></svg
								>
							</button>
						</li>
					{/each}
				</ul>
			{:else}
				<p class="muted hint">Every service, newest first. Press <kbd>/</kbd> to search.</p>
			{/if}
			<div class="controls">
				<p class="num count" aria-live="polite">
					{#if loading}Searching…{:else if !error}<strong>{count(rows.length)}{next ? '+' : ''}</strong> {view}{/if}
				</p>
				<div role="group" aria-label="View" class="seg-group">
					<button class="seg" type="button" aria-pressed={view === 'services'} onclick={() => nav({ view: 'services' })}>Services</button>
					<button class="seg" type="button" aria-pressed={view === 'hosts'} onclick={() => nav({ view: 'hosts' })}>Hosts</button>
				</div>
				<div role="group" aria-label="Layout" class="seg-group">
					<button class="seg" type="button" aria-pressed={layout === 'list'} aria-label="List" onclick={() => nav({ layout: 'list' })}>
						<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true"
							><path d="M4 6h16M4 12h16M4 18h16" /></svg
						>
					</button>
					<button class="seg" type="button" aria-pressed={layout === 'grid'} aria-label="Screenshot grid" onclick={() => nav({ layout: 'grid' })}>
						<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true"
							><rect x="4" y="4" width="7" height="7" rx="1" /><rect x="13" y="4" width="7" height="7" rx="1" /><rect x="4" y="13" width="7" height="7" rx="1" /><rect
								x="13"
								y="13"
								width="7"
								height="7"
								rx="1"
							/></svg
						>
					</button>
				</div>
			</div>
		</div>

		{#if fields.length}
			<details class="fields">
				<summary>Fields</summary>
				<div class="field-grid">
					{#each fields as f (f.name)}
						<button type="button" class="field" onclick={() => { draft = (draft ? draft + ' ' : '') + f.name + ':'; input.focus(); }}>
							<span class="mono">{f.name}</span>
							<span class="muted">{f.doc}</span>
						</button>
					{/each}
				</div>
			</details>
		{/if}
	</section>

	{#if error}
		<div class="error" role="alert">
			{error.message}
			{#if errSuggest}
				· <a href={'/' + qs({ q: errSuggest, view: view === 'services' ? '' : view })}>Did you mean <span class="mono">{error.body.suggest}</span>?</a>
			{/if}
		</div>
	{/if}

	<div class="body">
		<aside aria-label="Facets" class="facets">
			{#each facetOrder as [k, title] (k)}
				{#if facets[k]?.length}
					<section>
						<h2 class="label">{title}</h2>
						{#each facets[k] as v (v.term)}
							<button class="facet" type="button" aria-pressed={active.has(v.term)} onclick={() => nav({ q: toggle(q, v.term) })}>
								<span class="v">{label(v.value)}</span>
								<span class="num c">{count(v.count)}</span>
							</button>
						{/each}
					</section>
				{/if}
			{/each}
		</aside>

		<section aria-label="Results" class="results" class:dim={loading}>
			{#if !loading && !error && rows.length === 0}
				<p class="empty">Nothing matches. Remove a term, or check the hosts view.</p>
			{/if}
			{#if layout === 'list'}
				{#each rows as r (r.asset_id)}
					<a class="row" href="/assets/{r.asset_id}">
						<Shot thumb={r.thumbnail} />
						<div class="row-main">
							<div class="row-top">
								<span class="mono key">{key(r)}</span>
								<Status code={r.status_code} />
								{#if r.triage === 'new'}<span class="new"><span class="dot" aria-hidden="true"></span>New</span>{/if}
								{#if r.triage === 'interesting'}<span class="badge info">Interesting</span>{/if}
								{#if r.lifecycle !== 'active'}<Lifecycle state={r.lifecycle} />{/if}
							</div>
							{#if r.title}<p class="title">{r.title}</p>{/if}
							{#if r.final_url}<p class="mono final">→ {r.final_url}</p>{/if}
							{#if r.services?.length}
								<p class="mono final">{r.services.length} service{r.services.length === 1 ? '' : 's'}: {r.services.slice(0, 6).join(' · ')}</p>
							{/if}
							{#if r.technologies.length}
								<ul class="techs" aria-label="Technologies">
									{#each r.technologies as t (t)}
										{@const tv = splitTech(t)}
										<li class="chip">{tv.name}{#if tv.version}<span class="ver">{tv.version}</span>{/if}</li>
									{/each}
								</ul>
							{/if}
							<p class="meta">
								<span>{r.program}</span>
								{#if r.cdn || r.waf}<span>{[r.cdn, r.waf].filter(Boolean).join(' · ')}</span>{/if}
								{#if r.server}<span class="mono">{r.server}</span>{/if}
								<span>first seen {ago(r.first_seen)}</span>
							</p>
						</div>
					</a>
				{/each}
			{:else}
				<div class="grid">
					{#each rows as r (r.asset_id)}
						<a class="card" href="/assets/{r.asset_id}">
							<Shot thumb={r.thumbnail} width={320} />
							<div class="card-body">
								<div class="row-top">
									<span class="mono key small">{key(r)}</span>
									<Status code={r.status_code} />
								</div>
								<p class="title small">{r.title ?? ''}</p>
							</div>
						</a>
					{/each}
				</div>
			{/if}
			{#if next && !loading}<More onmore={loadMore} busy={more} />{/if}
		</section>
	</div>
</div>

<style>
	.query-area {
		display: flex;
		flex-direction: column;
		gap: 12px;
	}
	.query-row {
		display: flex;
		gap: 12px;
		flex-wrap: wrap;
		align-items: center;
	}
	.query {
		flex: 999 1 520px;
		min-width: 0;
		display: flex;
		align-items: center;
		gap: 10px;
		height: 44px;
		padding: 0 12px;
		border-radius: 10px;
		background: var(--raised);
		border: 1px solid oklch(0.34 0.008 260);
		color: var(--faint);
	}
	.query:focus-within {
		border-color: var(--accent);
	}
	.query input {
		flex: 1 1 auto;
		min-width: 0;
		height: 100%;
		border: 0;
		outline: 0;
		background: transparent;
		color: var(--text-strong);
		font-family: var(--mono);
		font-size: 14px;
	}
	.query input::placeholder {
		color: var(--faint);
	}
	kbd {
		font-family: var(--mono);
		font-size: 11px;
		color: var(--faint);
		border: 1px solid oklch(0.34 0.008 260);
		border-radius: 4px;
		padding: 1px 5px;
	}
	.actions {
		display: flex;
		gap: 8px;
		flex-wrap: wrap;
		align-items: center;
	}
	.menu {
		position: relative;
	}
	.menu summary {
		list-style: none;
	}
	.menu summary::-webkit-details-marker {
		display: none;
	}
	.menu-list {
		position: absolute;
		right: 0;
		top: calc(100% + 6px);
		z-index: 10;
		min-width: 160px;
		padding: 4px;
		border-radius: 10px;
		background: var(--raised);
		box-shadow: var(--shadow);
		display: flex;
		flex-direction: column;
	}
	.menu-list a {
		padding: 8px 10px;
		border-radius: 6px;
		color: var(--text);
		text-decoration: none;
		font-size: 13px;
	}
	.save {
		display: flex;
		gap: 8px;
		align-items: center;
		flex-wrap: wrap;
	}
	.save label {
		font-size: 13px;
		color: var(--muted);
	}
	.query-meta {
		display: flex;
		gap: 12px;
		flex-wrap: wrap;
		align-items: center;
		justify-content: space-between;
	}
	.terms {
		list-style: none;
		margin: 0;
		padding: 0;
		display: flex;
		gap: 6px;
		flex-wrap: wrap;
	}
	.terms li {
		display: inline-flex;
		align-items: center;
		gap: 4px;
		height: 26px;
		padding: 0 4px 0 10px;
		border-radius: 999px;
		background: oklch(0.22 0.007 260);
		box-shadow: var(--ring);
		font-family: var(--mono);
		font-size: 12px;
		color: oklch(0.88 0.008 260);
	}
	.chip-x {
		width: 20px;
		height: 20px;
		border-radius: 999px;
		border: 0;
		background: transparent;
		color: var(--faint);
		display: grid;
		place-items: center;
		cursor: pointer;
		padding: 0;
	}
	.hint {
		margin: 0;
		font-size: 13px;
	}
	.controls {
		display: flex;
		gap: 12px;
		align-items: center;
		flex-wrap: wrap;
	}
	.count {
		margin: 0;
		font-size: 13px;
		color: var(--muted);
	}
	.count strong {
		color: var(--text-strong);
		font-weight: 600;
	}
	.fields summary {
		font-size: 12px;
		color: var(--faint);
		cursor: pointer;
		width: max-content;
	}
	.field-grid {
		margin-top: 8px;
		display: grid;
		grid-template-columns: repeat(auto-fill, minmax(260px, 1fr));
		gap: 2px 12px;
	}
	.field {
		font: inherit;
		font-size: 12px;
		text-align: start;
		display: flex;
		gap: 8px;
		padding: 4px 6px;
		border: 0;
		border-radius: 6px;
		background: transparent;
		color: var(--text-body);
		cursor: pointer;
		min-width: 0;
	}
	.field .muted {
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.body {
		display: flex;
		gap: 32px;
		flex-wrap: wrap;
		align-items: flex-start;
	}
	.facets {
		flex: 1 1 220px;
		max-width: 260px;
		display: flex;
		flex-direction: column;
		gap: 24px;
	}
	.facets section {
		display: flex;
		flex-direction: column;
		gap: 2px;
	}
	.facets h2 {
		margin: 0 0 4px 8px;
	}
	.facet {
		font: inherit;
		font-size: 13px;
		display: flex;
		justify-content: space-between;
		gap: 12px;
		height: 30px;
		padding: 0 8px;
		border-radius: 6px;
		border: 0;
		background: transparent;
		color: var(--text-body);
		cursor: pointer;
		text-align: start;
		align-items: center;
	}
	.facet[aria-pressed='true'] {
		background: oklch(0.26 0.04 162);
		color: var(--text-strong);
	}
	.facet .v {
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.facet .c {
		font-size: 12px;
		color: var(--faint);
	}
	.results {
		flex: 999 1 560px;
		min-width: 0;
		display: flex;
		flex-direction: column;
		gap: 4px;
	}
	.results.dim {
		opacity: 0.6;
	}
	.row {
		display: flex;
		gap: 16px;
		padding: 12px;
		border-radius: 10px;
		text-decoration: none;
		color: inherit;
	}
	.row-main {
		flex: 1 1 auto;
		min-width: 0;
		display: flex;
		flex-direction: column;
		gap: 6px;
	}
	.row-top {
		display: flex;
		gap: 10px;
		align-items: center;
		flex-wrap: wrap;
	}
	.key {
		font-size: 14px;
		font-weight: 500;
		color: var(--text-strong);
		overflow-wrap: anywhere;
	}
	.key.small {
		font-size: 12px;
	}
	.new {
		display: inline-flex;
		align-items: center;
		gap: 5px;
		font-size: 12px;
		font-weight: 500;
		color: var(--info-text);
	}
	.title {
		margin: 0;
		font-size: 14px;
		color: var(--text-body);
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.title.small {
		font-size: 12px;
		min-height: 18px;
	}
	.final {
		margin: 0;
		font-size: 12px;
		color: var(--faint);
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.techs {
		list-style: none;
		margin: 0;
		padding: 0;
		display: flex;
		gap: 6px;
		flex-wrap: wrap;
	}
	.ver {
		color: var(--faint);
		margin-left: 2px;
	}
	.meta {
		margin: 0;
		display: flex;
		gap: 4px 14px;
		flex-wrap: wrap;
		font-size: 12px;
		color: var(--faint);
	}
	.grid {
		display: grid;
		grid-template-columns: repeat(auto-fill, minmax(260px, 1fr));
		gap: 16px;
	}
	.card {
		display: flex;
		flex-direction: column;
		gap: 8px;
		text-decoration: none;
		color: inherit;
		border-radius: 10px;
		padding: 8px;
		min-width: 0;
	}
	.card :global(img),
	.card :global(.none) {
		width: 100% !important;
		height: auto !important;
		aspect-ratio: 16 / 10;
	}
	.card-body {
		display: flex;
		flex-direction: column;
		gap: 4px;
		padding: 0 4px 4px;
		min-width: 0;
	}
	@media (hover: hover) {
		.row:hover,
		.card:hover {
			background: var(--surface);
		}
		.facet:hover,
		.field:hover,
		.menu-list a:hover {
			background: var(--control);
		}
		.chip-x:hover {
			color: var(--text-strong);
			background: var(--pressed);
		}
	}
	/* A wide screen keeps the query and the facets in view: facets and
	   results each scroll on their own. */
	@media (min-width: 721px) {
		.search {
			height: 100%;
			padding-bottom: 0;
		}
		.query-area,
		.search > .error {
			flex: none;
		}
		.body {
			flex: 1 1 auto;
			min-height: 0;
			flex-wrap: nowrap;
			align-items: stretch;
		}
		.facets,
		.results {
			min-height: 0;
			overflow-y: auto;
			overscroll-behavior: contain;
			padding-bottom: 32px;
		}
		.facets {
			flex: 0 0 240px;
			scrollbar-width: thin;
		}
	}
	@media (max-width: 720px) {
		.row :global(img),
		.row :global(.none) {
			display: none;
		}
		.facets {
			max-width: none;
			order: 2;
		}
	}
</style>
