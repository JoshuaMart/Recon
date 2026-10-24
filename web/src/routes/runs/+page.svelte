<script lang="ts">
	import { onMount } from 'svelte';
	import { api } from '#lib/api.ts';
	import { ago, dateTime, duration, label } from '#lib/format.ts';
	import type { Run } from '#lib/types.ts';

	let runs = $state<Run[]>([]);
	let loaded = $state(false);
	let kind = $state<'all' | 'recon' | 'verification'>('all');

	async function load() {
		runs = (await api.get<{ runs: Run[] }>('/api/runs')).runs;
		loaded = true;
	}

	onMount(() => {
		load();
		const t = setInterval(() => {
			if (runs.some((r) => r.state === 'queued' || r.state === 'running')) load();
		}, 10_000);
		return () => clearInterval(t);
	});

	const rows = $derived(runs.filter((r) => kind === 'all' || r.kind === kind));
	const tone: Record<string, string> = { succeeded: 'ok', running: 'info', queued: 'info', failed: 'bad', timed_out: 'bad' };
	const n = (r: Run, k: string) => (r.ingest?.[k] as number | undefined) ?? 0;
</script>

<svelte:head><title>Runs · Recon</title></svelte:head>

<div class="page">
	<div class="page-head">
		<div>
			<h1>Runs</h1>
			<p>The last 100 FastRecon runs: recon on a wildcard, verification of known hosts.</p>
		</div>
		<div role="group" aria-label="Kind" class="seg-group">
			<button class="seg" type="button" aria-pressed={kind === 'all'} onclick={() => (kind = 'all')}>All</button>
			<button class="seg" type="button" aria-pressed={kind === 'recon'} onclick={() => (kind = 'recon')}>Recon</button>
			<button class="seg" type="button" aria-pressed={kind === 'verification'} onclick={() => (kind = 'verification')}>Verification</button>
		</div>
	</div>

	<div class="table-wrap">
		<table>
			<thead>
				<tr>
					<th scope="col">Run</th>
					<th scope="col">State</th>
					<th scope="col">Trigger</th>
					<th scope="col">Queued</th>
					<th scope="col" class="end">Took</th>
					<th scope="col" class="end">Hosts</th>
					<th scope="col" class="end">New assets</th>
					<th scope="col" class="end">Changes</th>
				</tr>
			</thead>
			<tbody>
				{#each rows as r (r.id)}
					<tr>
						<td>
							<a class="name" href="/runs/{r.id}">{r.kind === 'recon' ? 'Recon' : 'Verification'} {#if r.apex}<span class="mono">*.{r.apex}</span>{:else}<span class="muted">{r.stage}</span>{/if}</a>
							{#if r.error}<div class="err small">{r.error}</div>{/if}
						</td>
						<td><span class="badge {tone[r.state] ?? ''}">{label(r.state)}</span>{#if r.ingest?.degraded}<span class="badge warn">degraded</span>{/if}</td>
						<td class="muted">{r.trigger}</td>
						<td class="muted num" title={dateTime(r.queued_at)}>{ago(r.queued_at)}</td>
						<td class="num end">{duration(r.started_at, r.finished_at)}</td>
						<td class="num end">{r.ingest ? n(r, 'hosts') : '—'}</td>
						<td class="num end">{r.ingest ? n(r, 'assets_created') + n(r, 'services_created') : '—'}</td>
						<td class="num end">{r.ingest ? n(r, 'changes') : '—'}</td>
					</tr>
				{:else}
					<tr><td colspan="8" class="empty">{loaded ? 'No run yet. Scan a wildcard to start one.' : 'Loading…'}</td></tr>
				{/each}
			</tbody>
		</table>
	</div>
</div>

<style>
	table {
		min-width: 860px;
	}
	.name {
		color: var(--text-strong);
		text-decoration: none;
		font-weight: 500;
	}
	.small {
		font-size: 12px;
	}
	.err {
		color: oklch(0.8 0.1 25);
	}
	td .badge + .badge {
		margin-left: 6px;
	}
	@media (hover: hover) {
		.name:hover {
			color: var(--accent-text);
		}
	}
</style>
