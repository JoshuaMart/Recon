<script lang="ts">
	import { onMount } from 'svelte';
	import { api, qs } from '#lib/api.ts';
	import { ago, dateTime, displayKey, label } from '#lib/format.ts';
	import type { Change, Program } from '#lib/types.ts';
	import More from '#lib/components/More.svelte';
	import Combobox from '#lib/components/Combobox.svelte';

	let changes = $state<Change[]>([]);
	let next = $state<string | undefined>();
	let programs = $state<Program[]>([]);
	let program = $state('');
	let loaded = $state(false);
	let more = $state(false);
	// Only the answer to the latest filter is shown: a slow one for the
	// previous program is dropped.
	let ticket = 0;

	async function load() {
		const t = ++ticket;
		loaded = false;
		const r = await api.get<{ changes: Change[]; next?: string }>('/api/changes' + qs({ program }));
		if (t !== ticket) return;
		changes = r.changes;
		next = r.next;
		loaded = true;
	}

	async function loadMore() {
		if (!next || more) return;
		more = true;
		const t = ticket;
		try {
			const r = await api.get<{ changes: Change[]; next?: string }>('/api/changes' + qs({ program, cursor: next }));
			if (t !== ticket) return;
			changes = [...changes, ...r.changes];
			next = r.next;
		} finally {
			more = false;
		}
	}

	onMount(() => {
		api.get<{ programs: Program[] }>('/api/programs').then((r) => (programs = r.programs.sort((a, b) => a.name.localeCompare(b.name))));
	});

	$effect(() => {
		void program;
		load();
	});

	const programOptions = $derived(programs.map((p) => ({ value: p.id, label: p.name, hint: p.origin === 'manual' ? 'manual' : (p.platform ?? '') })));

	const tone: Record<string, string> = { new_asset: 'info', lifecycle: 'warn', scope: 'warn' };

	function fmt(v: unknown): string {
		if (v === null || v === undefined) return '';
		if (typeof v !== 'object') return String(v);
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
	<div class="page-head">
		<div>
			<h1>Changes</h1>
			<p>What moved in the inventory, newest first.</p>
		</div>
		<Combobox id="program" label="Program" placeholder="Every program" options={programOptions} bind:value={program} />
	</div>

	<div class="table-wrap">
		<table>
			<thead>
				<tr><th scope="col">When</th><th scope="col">Change</th><th scope="col">Asset</th><th scope="col">Program</th><th scope="col">Detail</th></tr>
			</thead>
			<tbody>
				{#each changes as c (c.id + c.at)}
					<tr>
						<td class="muted num nowrap" title={dateTime(c.at)}>{ago(c.at)}</td>
						<td class="nowrap"><span class="badge {tone[c.kind] ?? ''}">{label(c.kind)}</span>{#if c.layer}<span class="muted small"> {c.layer}</span>{/if}</td>
						<td><a class="mono" href="/assets/{c.asset_id}">{key(c)}</a></td>
						<td class="muted">{c.program}</td>
						<td class="mono small detail">{detail(c)}</td>
					</tr>
				{:else}
					<tr><td colspan="5" class="empty">{loaded ? 'Nothing changed yet.' : 'Loading…'}</td></tr>
				{/each}
			</tbody>
		</table>
		{#if next && loaded}<More onmore={loadMore} busy={more} />{/if}
	</div>
</div>

<style>
	table {
		min-width: 860px;
	}
	/* A wide screen keeps the title, the filter and the column names in view:
	   only the rows scroll. */
	@media (min-width: 721px) {
		.feed {
			height: 100%;
			padding-bottom: 24px;
		}
		.feed > .page-head {
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
</style>
