<script lang="ts">
	import { onMount } from 'svelte';
	import { api, ApiError, qs } from '#lib/api.ts';
	import { ago, count, daysSince, label } from '#lib/format.ts';
	import type { Run, Wildcard } from '#lib/types.ts';
	import Combobox from '#lib/components/Combobox.svelte';

	let wildcards = $state<Wildcard[]>([]);
	let runs = $state<Run[]>([]);
	let loaded = $state(false);
	let error = $state('');
	let notice = $state('');
	let filter = $state<'all' | 'never' | 'old'>('all');
	let program = $state('');
	let platform = $state('');
	let oldest = $state(10);
	let busy = $state(false);

	async function load() {
		try {
			const [w, r] = await Promise.all([
				api.get<{ wildcards: Wildcard[] }>('/api/wildcards' + qs({ sort: 'oldest' })),
				api.get<{ runs: Run[] }>('/api/runs')
			]);
			wildcards = w.wildcards;
			runs = r.runs;
			loaded = true;
		} catch (e) {
			error = String(e);
		}
	}

	onMount(() => {
		load();
		// While a recon is queued or running, its row follows it.
		const t = setInterval(() => {
			if (runs.some((r) => r.kind === 'recon' && (r.state === 'queued' || r.state === 'running'))) load();
		}, 10_000);
		return () => clearInterval(t);
	});

	const inFlight = $derived(
		new Map(runs.filter((r) => r.wildcard_id && (r.state === 'queued' || r.state === 'running')).map((r) => [r.wildcard_id!, r]))
	);
	const programs = $derived(
		[...new Map(wildcards.map((w) => [w.program_id, w.program_name])).entries()].sort((a, b) => a[1].localeCompare(b[1]))
	);
	const programOptions = $derived(
		programs.map(([id, name]) => {
			const n = wildcards.filter((w) => w.program_id === id).length;
			return { value: id, label: name, hint: `${n} wildcard${n === 1 ? '' : 's'}` };
		})
	);
	const platforms = $derived([...new Set(wildcards.map((w) => w.platform ?? 'manual'))].sort());
	const scoped = $derived(
		wildcards.filter((w) => (!program || w.program_id === program) && (!platform || (w.platform ?? 'manual') === platform))
	);
	const rows = $derived(
		scoped.filter((w) => {
			const d = daysSince(w.last_recon_at);
			if (filter === 'never') return d === null;
			if (filter === 'old') return d !== null && d > 30;
			return true;
		})
	);
	// What the button will queue: the oldest rows shown, not already in flight.
	const toScan = $derived(Math.min(Math.max(0, Math.floor(Number(oldest) || 0)), rows.filter((w) => !inFlight.has(w.id)).length));
	const never = $derived(scoped.filter((w) => !w.last_recon_at).length);

	const recon = (w: Wildcard) => api.post<{ queued: boolean; reason?: string }>(`/api/wildcards/${w.id}/recon`);

	async function scan(w: Wildcard) {
		notice = '';
		try {
			const r = await recon(w);
			notice = r.queued ? `Recon queued on ${w.apex}.` : `${w.apex}: ${r.reason}.`;
			await load();
		} catch (err) {
			notice = `${w.apex}: ${err instanceof ApiError ? err.message : String(err)}`;
		}
	}

	// The oldest of what the table shows, whatever the filters: the button
	// never queues a wildcard filtered out.
	async function scanOldest(e: SubmitEvent) {
		e.preventDefault();
		busy = true;
		notice = '';
		try {
			const picked = rows.filter((w) => !inFlight.has(w.id)).slice(0, toScan);
			let n = 0;
			for (const w of picked) if ((await recon(w)).queued) n++;
			notice = n ? `${n} recon${n === 1 ? '' : 's'} queued, oldest first.` : 'Nothing queued: every wildcard shown is already running or queued.';
			await load();
		} catch (err) {
			notice = String(err);
		} finally {
			busy = false;
		}
	}

	function tone(w: Wildcard): string {
		const d = daysSince(w.last_recon_at);
		if (d === null) return 'never';
		return d > 30 ? 'stale' : 'fresh';
	}
</script>

<svelte:head><title>Wildcards · Recon</title></svelte:head>

<div class="page">
	<div class="page-head">
		<div>
			<h1>Wildcards</h1>
			<p class="num">
				{count(scoped.length)} wildcards across {count(new Set(scoped.map((w) => w.program_id)).size)} programs · {count(never)} never scanned · {count(inFlight.size)} recon{inFlight.size === 1 ? '' : 's'} in flight
			</p>
		</div>
		<form class="oldest" onsubmit={scanOldest}>
			<label for="n">How many</label>
			<input id="n" class="input num" type="number" inputmode="numeric" min="1" max="500" required bind:value={oldest} />
			<button class="btn btn-primary" type="submit" disabled={busy || toScan === 0}>
				{busy ? 'Queuing…' : `Scan the ${toScan} oldest`}
			</button>
		</form>
	</div>

	<div class="filters">
		<div role="group" aria-label="Filter" class="seg-group">
			<button class="seg" type="button" aria-pressed={filter === 'all'} onclick={() => (filter = 'all')}>All</button>
			<button class="seg" type="button" aria-pressed={filter === 'never'} onclick={() => (filter = 'never')}>Never scanned</button>
			<button class="seg" type="button" aria-pressed={filter === 'old'} onclick={() => (filter = 'old')}>Older than 30 days</button>
		</div>
		<Combobox id="program" label="Program" placeholder="Every program" options={programOptions} bind:value={program} />
		<label class="sr-only" for="platform">Platform</label>
		<select id="platform" class="select" bind:value={platform}>
			<option value="">Every platform</option>
			{#each platforms as p (p)}<option value={p}>{p}</option>{/each}
		</select>
		<p class="muted hint">Oldest first · auto-scan follows the program, then the global default</p>
	</div>

	{#if notice}<p class="notice" role="status">{notice}</p>{/if}
	{#if error}<p class="error" role="alert">{error}</p>{/if}

	<div class="table-wrap">
		<table>
			<thead>
				<tr>
					<th scope="col">Wildcard</th>
					<th scope="col">Program</th>
					<th scope="col">Last recon</th>
					<th scope="col" class="end">Assets</th>
					<th scope="col">Auto-scan</th>
					<th scope="col"><span class="sr-only">Action</span></th>
				</tr>
			</thead>
			<tbody>
				{#each rows as w (w.id)}
					{@const run = inFlight.get(w.id)}
					<tr>
						<td class="mono strong">*.{w.apex}</td>
						<td><a class="plain" href="/programs/{w.program_id}">{w.program_name}</a></td>
						<td class="num {tone(w)}" title={w.last_recon_at ?? ''}>{ago(w.last_recon_at)}</td>
						<td class="num end">
							{#if w.assets_found}<a class="plain" href={'/' + qs({ q: 'apex:' + w.apex, view: 'hosts' })}>{count(w.assets_found)}</a>{:else}—{/if}
						</td>
						<td class="muted">{label(w.auto_scan)}</td>
						<td class="end">
							{#if run}
								<a class="running" href="/runs/{run.id}"><span class="dot" aria-hidden="true"></span>{run.state === 'running' ? 'Running' : 'Queued'}</a>
							{:else}
								<button class="btn btn-sm" type="button" aria-label="Scan {w.apex}" onclick={() => scan(w)}>Scan</button>
							{/if}
						</td>
					</tr>
				{:else}
					<tr><td colspan="6" class="empty">{loaded ? 'No wildcard matches.' : 'Loading…'}</td></tr>
				{/each}
			</tbody>
		</table>
	</div>
</div>

<style>
	.oldest {
		display: flex;
		gap: 8px;
		align-items: center;
	}
	.oldest label {
		font-size: 13px;
		color: var(--muted);
	}
	.oldest input {
		width: 64px;
		text-align: end;
		appearance: textfield;
	}
	.oldest input::-webkit-inner-spin-button,
	.oldest input::-webkit-outer-spin-button {
		appearance: none;
		margin: 0;
	}
	.filters {
		display: flex;
		gap: 12px;
		align-items: center;
		flex-wrap: wrap;
	}
	.hint {
		margin: 0 0 0 auto;
		font-size: 12px;
	}
	table {
		min-width: 820px;
	}
	.strong {
		color: var(--text-strong);
	}
	.plain {
		color: var(--text-body);
		text-decoration: none;
	}
	.never {
		color: var(--warn);
	}
	.stale {
		color: var(--text-body);
	}
	.fresh {
		color: var(--muted);
	}
	.running {
		display: inline-flex;
		align-items: center;
		gap: 6px;
		font-size: 12px;
		font-weight: 500;
		color: var(--info-text);
		text-decoration: none;
	}
	@media (hover: hover) {
		.plain:hover {
			color: var(--accent-text);
		}
	}
</style>
