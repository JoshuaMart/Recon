<script lang="ts">
	import { goto } from '$app/navigation';
	import { onMount } from 'svelte';
	import { api, ApiError } from '#lib/api.ts';
	import { ago, count, label } from '#lib/format.ts';
	import type { Program } from '#lib/types.ts';

	interface Sync {
		programs?: Record<string, number>;
		[k: string]: unknown;
	}

	let programs = $state<Program[]>([]);
	let loaded = $state(false);
	let filter = $state<'active' | 'paused' | 'archived' | 'all'>('active');
	let text = $state('');
	let creating = $state(false);
	let name = $state('');
	let platform = $state('');
	let error = $state('');
	let notice = $state('');
	let syncing = $state(false);

	async function load() {
		programs = (await api.get<{ programs: Program[] }>('/api/programs')).programs;
		loaded = true;
	}
	onMount(load);

	const rows = $derived(
		programs
			.filter((p) => filter === 'all' || p.state === filter)
			.filter((p) => !text || p.name.toLowerCase().includes(text.toLowerCase()) || (p.platform ?? '').includes(text.toLowerCase()))
			.sort((a, b) => a.name.localeCompare(b.name))
	);
	const by = $derived((s: string) => programs.filter((p) => p.state === s).length);

	async function create(e: SubmitEvent) {
		e.preventDefault();
		error = '';
		try {
			const p = await api.post<Program>('/api/programs', { name, ...(platform ? { platform } : {}) });
			await goto(`/programs/${p.id}`);
		} catch (err) {
			error = err instanceof ApiError ? err.message : String(err);
		}
	}

	async function sync() {
		syncing = true;
		notice = '';
		try {
			const s = await api.post<Sync>('/api/sync');
			notice = 'Sync done: ' + Object.entries(s)
				.filter(([, v]) => typeof v === 'number')
				.map(([k, v]) => `${label(k)} ${v}`)
				.join(', ');
			await load();
		} catch (err) {
			notice = err instanceof ApiError ? err.message : String(err);
		} finally {
			syncing = false;
		}
	}
</script>

<svelte:head><title>Programs · Recon</title></svelte:head>

<div class="page">
	<div class="page-head">
		<div>
			<h1>Programs</h1>
			<p class="num">{count(by('active'))} active · {count(by('paused'))} paused · {count(by('archived'))} archived</p>
		</div>
		<div class="actions">
			<button class="btn" type="button" onclick={sync} disabled={syncing}>{syncing ? 'Syncing…' : 'Sync now'}</button>
			<button class="btn btn-primary" type="button" onclick={() => (creating = !creating)} aria-expanded={creating}>New program</button>
		</div>
	</div>

	{#if creating}
		<form class="panel create" onsubmit={create}>
			<h2>A manual program</h2>
			<div class="fields">
				<label>Name <input class="input" bind:value={name} required placeholder="MSRC" /></label>
				<label>Platform <input class="input" bind:value={platform} placeholder="optional" /></label>
				<button class="btn btn-primary" type="submit" disabled={!name.trim()}>Create</button>
			</div>
			<p class="muted small">Its scope is the rules you add next. The sync never touches it.</p>
			{#if error}<p class="error">{error}</p>{/if}
		</form>
	{/if}
	{#if notice}<p class="notice" role="status">{notice}</p>{/if}

	<div class="filters">
		<div role="group" aria-label="State" class="seg-group">
			{#each ['active', 'paused', 'archived', 'all'] as const as s (s)}
				<button class="seg" type="button" aria-pressed={filter === s} onclick={() => (filter = s)}>{s === 'all' ? 'All' : label(s).replace(/^./, (c) => c.toUpperCase())}</button>
			{/each}
		</div>
		<label class="sr-only" for="find">Find a program</label>
		<input id="find" class="input" type="search" placeholder="Find a program" bind:value={text} />
	</div>

	<div class="table-wrap">
		<table>
			<thead>
				<tr>
					<th scope="col">Program</th>
					<th scope="col">Origin</th>
					<th scope="col">State</th>
					<th scope="col" class="end">Rules</th>
					<th scope="col" class="end">Wildcards</th>
					<th scope="col">Auto-scan</th>
					<th scope="col">Updated</th>
				</tr>
			</thead>
			<tbody>
				{#each rows as p (p.id)}
					<tr>
						<td><a class="name" href="/programs/{p.id}">{p.name}</a>{#if p.bounty === false}<span class="muted small"> · VDP</span>{/if}</td>
						<td class="muted">{p.origin === 'manual' ? 'manual' : p.platform ?? p.origin}</td>
						<td><span class="badge {p.state === 'active' ? 'ok' : p.state === 'paused' ? 'warn' : ''}">{p.state}</span></td>
						<td class="num end">{p.open_rules ?? '—'}</td>
						<td class="num end">{p.wildcards ?? '—'}</td>
						<td class="muted">{label(p.effective_auto_scan)}{p.auto_scan ? '' : ' (default)'}</td>
						<td class="muted num">{ago(p.updated_at)}</td>
					</tr>
				{:else}
					<tr><td colspan="7" class="empty">{loaded ? 'No program matches.' : 'Loading…'}</td></tr>
				{/each}
			</tbody>
		</table>
	</div>
</div>

<style>
	.actions,
	.filters {
		display: flex;
		gap: 8px;
		align-items: center;
		flex-wrap: wrap;
	}
	.filters .input {
		width: 240px;
	}
	.create .fields {
		display: flex;
		gap: 12px;
		align-items: flex-end;
		flex-wrap: wrap;
	}
	.create label {
		display: flex;
		flex-direction: column;
		gap: 4px;
		font-size: 13px;
		color: var(--muted);
	}
	.small {
		font-size: 12px;
		margin: 0;
	}
	table {
		min-width: 760px;
	}
	.name {
		color: var(--text-strong);
		text-decoration: none;
		font-weight: 500;
	}
	@media (hover: hover) {
		.name:hover {
			color: var(--accent-text);
		}
	}
</style>
