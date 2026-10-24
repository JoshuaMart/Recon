<script lang="ts">
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { onMount } from 'svelte';
	import { api, ApiError, qs } from '#lib/api.ts';
	import { ago, count, dateTime, displayKey, label } from '#lib/format.ts';
	import { quote, terms, toggle, without } from '#lib/query.ts';
	import type { Change, Program } from '#lib/types.ts';
	import { refreshUnseen, unseen } from '#lib/unseen.svelte.ts';
	import More from '#lib/components/More.svelte';
	import Combobox from '#lib/components/Combobox.svelte';

	interface FeedPage {
		query: string;
		changes: Change[];
		next?: string;
	}

	const kinds = [
		'new_asset',
		'lifecycle_changed',
		'scope_changed',
		'dns_changed',
		'port_opened',
		'port_closed',
		'http_changed',
		'tech_added',
		'tech_removed',
		'tech_version_changed',
		'render_changed'
	];

	const q = $derived(page.url.searchParams.get('q') ?? '');
	const view = $derived(page.url.searchParams.get('view') === 'all' ? 'all' : 'unseen');
	const run = $derived(page.url.searchParams.get('run') ?? '');

	let draft = $state('');
	let changes = $state<Change[]>([]);
	let next = $state<string | undefined>();
	let programs = $state<Program[]>([]);
	let loaded = $state(false);
	let more = $state(false);
	let error = $state<ApiError | null>(null);
	let notice = $state('');
	let marking = $state(false);
	// Only the answer to the latest read is shown: a slow one for the previous
	// query is dropped.
	let ticket = 0;

	function nav(params: { q?: string; view?: string; run?: string }) {
		const p = { q, view, run, ...params };
		goto('/changes' + qs({ q: p.q, view: p.view === 'unseen' ? '' : p.view, run: p.run }), { reset: false });
	}

	async function load() {
		const t = ++ticket;
		loaded = false;
		error = null;
		try {
			const r = await api.get<FeedPage>('/api/changes' + qs({ q, view, run }));
			if (t !== ticket) return;
			changes = r.changes;
			next = r.next;
		} catch (e) {
			if (t !== ticket) return;
			error = e instanceof ApiError ? e : new ApiError(0, { detail: String(e) });
			changes = [];
			next = undefined;
		} finally {
			if (t === ticket) loaded = true;
		}
	}

	async function loadMore() {
		if (!next || more) return;
		more = true;
		const t = ticket;
		try {
			const r = await api.get<FeedPage>('/api/changes' + qs({ q, view, run, cursor: next }));
			if (t !== ticket) return;
			changes = [...changes, ...r.changes];
			next = r.next;
		} finally {
			more = false;
		}
	}

	async function markSeen() {
		marking = true;
		notice = '';
		try {
			const r = await api.post<{ assets: number }>('/api/changes/seen', { q, run });
			notice = `${count(r.assets)} asset${r.assets === 1 ? '' : 's'} marked as seen.`;
			await Promise.all([load(), refreshUnseen()]);
		} catch (e) {
			notice = e instanceof ApiError ? e.message : String(e);
		} finally {
			marking = false;
		}
	}

	onMount(() => {
		api.get<{ programs: Program[] }>('/api/programs').then((r) => (programs = r.programs));
	});

	$effect(() => {
		void q;
		void view;
		void run;
		draft = q;
		load();
	});

	const chips = $derived(terms(q));
	const kindTerm = $derived(chips.find((t) => t.startsWith('change:')) ?? '');
	const programOptions = $derived(
		programs
			.filter((p) => p.state !== 'archived')
			.map((p) => ({ value: p.name, label: p.name, hint: p.origin === 'manual' ? 'manual' : (p.platform ?? '') }))
			.sort((a, b) => a.label.localeCompare(b.label))
	);

	function setKind(k: string) {
		let next = kindTerm ? toggle(q, kindTerm) : q;
		if (k) next = toggle(next, 'change:' + k);
		nav({ q: next });
	}

	const tone: Record<string, string> = {
		new_asset: 'info',
		lifecycle_changed: 'warn',
		scope_changed: 'warn',
		port_opened: 'ok',
		port_closed: '',
		tech_version_changed: 'info'
	};

	function fmt(v: unknown): string {
		if (v === null || v === undefined) return '';
		if (typeof v !== 'object') return String(v);
		if (Array.isArray(v)) return v.join(', ');
		const o = v as Record<string, unknown>;
		const keys = Object.keys(o);
		if (keys.length <= 3) return keys.map((k) => `${k} ${typeof o[k] === 'object' ? JSON.stringify(o[k]) : o[k]}`).join(', ');
		return JSON.stringify(v);
	}

	function detail(c: Change): string {
		if (c.kind === 'new_asset') return '';
		const b = fmt(c.before);
		const a = fmt(c.after);
		return b ? `${b} → ${a}` : a;
	}

	const key = (c: Change) => displayKey(c.asset_kind, c.asset_key);
</script>

<svelte:head><title>Changes · Recon</title></svelte:head>

<div class="page feed">
	<div class="head">
		<div class="page-head">
			<div>
				<h1>Changes</h1>
				<p>What moved in the inventory, newest first. Opening an asset marks its changes as seen.</p>
			</div>
			<div class="actions">
				<div role="group" aria-label="View" class="seg-group">
					<button class="seg" type="button" aria-pressed={view === 'unseen'} onclick={() => nav({ view: 'unseen' })}>
						<!-- The global count: shown only when nothing narrows the feed. -->
						Unseen{#if unseen.count && !q && !run}<span class="num muted"> {unseen.count}{unseen.capped ? '+' : ''}</span>{/if}
					</button>
					<button class="seg" type="button" aria-pressed={view === 'all'} onclick={() => nav({ view: 'all' })}>All</button>
				</div>
				{#if view === 'unseen' && changes.length}
					<button class="btn" type="button" onclick={markSeen} disabled={marking}>{marking ? 'Marking…' : 'Mark as seen'}</button>
				{/if}
			</div>
		</div>

		{#if run}
			<p class="notice run">
				The changes one run caused. <a href="/runs/{run}">Open the run</a> ·
				<button type="button" class="link" onclick={() => nav({ run: '' })}>Show every run</button>
			</p>
		{/if}

		<div class="filters">
			<form
				class="query"
				role="search"
				onsubmit={(e) => {
					e.preventDefault();
					nav({ q: draft.trim() });
				}}
			>
				<label for="q" class="sr-only">Filter changes</label>
				<input id="q" class="mono" bind:value={draft} spellcheck="false" autocomplete="off" placeholder="tech:wordpress change:tech_version_changed" />
			</form>
			<label class="sr-only" for="kind">Kind of change</label>
			<select id="kind" class="select" value={kindTerm.slice('change:'.length)} onchange={(e) => setKind(e.currentTarget.value)}>
				<option value="">Any change</option>
				{#each kinds as k (k)}<option value={k}>{label(k)}</option>{/each}
				{#if kindTerm && !kinds.includes(kindTerm.slice('change:'.length))}
					<option value={kindTerm.slice('change:'.length)}>{label(kindTerm.slice('change:'.length))}</option>
				{/if}
			</select>
			<Combobox
				id="program"
				label="Filter by program"
				placeholder="Filter by program"
				options={programOptions}
				onpick={(o) => {
					const t = 'program:' + quote(o.value);
					if (!chips.includes(t)) nav({ q: toggle(q, t) });
				}}
			/>
		</div>

		{#if chips.length}
			<ul aria-label="Terms" class="terms">
				{#each chips as t, i (t + i)}
					<li>
						<span>{t}</span>
						<button type="button" class="chip-x" aria-label="Remove {t}" onclick={() => nav({ q: without(q, i) })}>×</button>
					</li>
				{/each}
			</ul>
		{/if}
		{#if error}<p class="error" role="alert">{error.message}</p>{/if}
		{#if notice}<p class="notice" role="status">{notice}</p>{/if}
	</div>

	<div class="table-wrap">
		<table>
			<thead>
				<tr>
					<th scope="col"><span class="sr-only">Unseen</span></th>
					<th scope="col">When</th>
					<th scope="col">Change</th>
					<th scope="col">Asset</th>
					<th scope="col">Program</th>
					<th scope="col">Detail</th>
				</tr>
			</thead>
			<tbody>
				{#each changes as c (c.id + c.at)}
					<tr class:seen={!c.unseen}>
						<td class="dotcell">{#if c.unseen}<span class="dot new" title="Unseen"></span><span class="sr-only">Unseen</span>{/if}</td>
						<td class="muted num nowrap" title={dateTime(c.at)}>{ago(c.at)}</td>
						<td class="nowrap">
							<span class="badge {tone[c.kind] ?? ''}">{label(c.kind)}</span>{#if c.layer}<span class="muted small"> {c.layer}</span>{/if}
						</td>
						<td><a class="mono" href="/assets/{c.asset_id}">{key(c)}</a></td>
						<td class="muted">{c.program}</td>
						<td class="mono small detail">{detail(c)}</td>
					</tr>
				{:else}
					<tr>
						<td colspan="6" class="empty">
							{#if !loaded}Loading…{:else if view === 'unseen'}Nothing unseen{q ? ' for this query' : ''}. <button type="button" class="link" onclick={() => nav({ view: 'all' })}>Show every change</button>{:else}No change{q ? ' matches' : ' yet'}.{/if}
						</td>
					</tr>
				{/each}
			</tbody>
		</table>
		{#if next && loaded}<More onmore={loadMore} busy={more} />{/if}
	</div>
</div>

<style>
	.head {
		display: flex;
		flex-direction: column;
		gap: 12px;
	}
	.actions,
	.filters {
		display: flex;
		gap: 10px;
		align-items: center;
		flex-wrap: wrap;
	}
	.query {
		flex: 999 1 360px;
		min-width: 0;
		display: flex;
		height: 34px;
		border-radius: 8px;
		background: var(--raised);
		border: 1px solid var(--line-strong);
	}
	.query:focus-within {
		border-color: var(--accent);
	}
	.query input {
		flex: 1;
		min-width: 0;
		border: 0;
		outline: 0;
		background: transparent;
		color: var(--text-strong);
		font-size: 13px;
		padding: 0 10px;
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
		height: 24px;
		padding: 0 4px 0 10px;
		border-radius: 999px;
		background: oklch(0.22 0.007 260);
		box-shadow: var(--ring);
		font-family: var(--mono);
		font-size: 12px;
	}
	.chip-x {
		border: 0;
		background: transparent;
		color: var(--faint);
		cursor: pointer;
		font-size: 14px;
		padding: 0 4px;
	}
	.link {
		font: inherit;
		border: 0;
		background: none;
		padding: 0;
		color: var(--accent-text);
		cursor: pointer;
		text-decoration: underline;
	}
	.run {
		margin: 0;
	}
	table {
		min-width: 900px;
	}
	.dotcell {
		width: 18px;
		padding-right: 0;
	}
	.dot.new {
		display: inline-block;
		color: var(--info);
	}
	tr.seen td {
		color: var(--muted);
	}
	.nowrap {
		white-space: nowrap;
	}
	.small {
		font-size: 12px;
	}
	.detail {
		color: var(--muted);
		max-width: 520px;
		overflow-wrap: anywhere;
	}
	/* A wide screen keeps the title, the filters and the column names in
	   view: only the rows scroll. */
	@media (min-width: 721px) {
		.feed {
			height: 100%;
			padding-bottom: 24px;
		}
		.head {
			flex: none;
		}
		.feed > .table-wrap {
			flex: 1 1 auto;
			min-height: 0;
			overflow: auto;
			overscroll-behavior: contain;
		}
		th {
			position: sticky;
			top: 0;
			z-index: 1;
			background: var(--surface);
		}
	}
</style>
