<script lang="ts">
	import { page } from '$app/state';
	import { api, ApiError } from '#lib/api.ts';
	import { ago, dateTime, duration, label } from '#lib/format.ts';
	import type { Run } from '#lib/types.ts';

	const id = $derived(page.params.id ?? '');
	let run = $state<Run | null>(null);
	let error = $state('');

	$effect(() => {
		api
			.get<Run>(`/api/runs/${id}`)
			.then((r) => (run = r))
			.catch((e) => (error = e instanceof ApiError && e.status === 404 ? 'No run has this id.' : String(e)));
	});

	const sources = $derived.by(() => {
		const s = run?.report?.sources;
		if (!s || typeof s !== 'object') return [];
		return Object.entries(s as Record<string, Record<string, unknown>>);
	});
	const scalar = (v: unknown) => (v === null || typeof v !== 'object' ? String(v) : JSON.stringify(v));
</script>

<svelte:head><title>Run · Recon</title></svelte:head>

<div class="page">
	<a class="back" href="/runs">← Runs</a>
	{#if error}
		<p class="error" role="alert">{error}</p>
	{:else if !run}
		<p class="muted">Loading…</p>
	{:else}
		<div class="page-head">
			<div>
				<h1>{run.kind === 'recon' ? 'Recon' : 'Verification'} {#if run.apex}<span class="mono">*.{run.apex}</span>{/if}</h1>
				<p>{label(run.state)} · {run.stage} · by {run.trigger} · queued {ago(run.queued_at)} · took {duration(run.started_at, run.finished_at)}</p>
			</div>
			<div class="head-actions">
				<a class="btn" href="/changes?view=all&run={run.id}">Its changes</a>
				<a class="btn" href="/programs/{run.program_id}">Its program</a>
			</div>
		</div>
		{#if run.error}<p class="error">{run.error}</p>{/if}

		<div class="cols">
			{#if run.ingest}
				<section class="panel" aria-label="Ingestion">
					<h2>Ingestion</h2>
					<dl class="kv num">
						{#each Object.entries(run.ingest) as [k, v] (k)}<dt>{label(k)}</dt><dd>{scalar(v)}</dd>{/each}
					</dl>
				</section>
			{/if}
			{#if sources.length}
				<section class="panel wide" aria-label="Sources">
					<h2>Per source</h2>
					<div class="table-wrap flat">
						<table>
							<thead><tr><th scope="col">Source</th>{#each Object.keys(sources[0][1]) as k (k)}<th scope="col">{label(k)}</th>{/each}</tr></thead>
							<tbody>
								{#each sources as [name, s] (name)}
									<tr><td class="mono">{name}</td>{#each Object.values(s) as v, i (i)}<td class="num">{scalar(v)}</td>{/each}</tr>
								{/each}
							</tbody>
						</table>
					</div>
				</section>
			{/if}
			{#if run.report}
				<section class="panel wide" aria-label="Report">
					<details>
						<summary class="label">Report metadata</summary>
						<pre class="mono">{JSON.stringify(run.report, null, 2)}</pre>
					</details>
				</section>
			{/if}
		</div>
		<p class="muted small">Queued {dateTime(run.queued_at)} · started {dateTime(run.started_at)} · finished {dateTime(run.finished_at)}</p>
	{/if}
</div>

<style>
	.back {
		font-size: 13px;
		text-decoration: none;
		width: max-content;
	}
	.head-actions {
		display: flex;
		gap: 8px;
	}
	.cols {
		display: flex;
		gap: 20px;
		flex-wrap: wrap;
		align-items: flex-start;
	}
	.cols .panel {
		flex: 1 1 300px;
	}
	.cols .wide {
		flex: 999 1 520px;
	}
	.flat {
		box-shadow: none;
		background: transparent;
	}
	summary {
		cursor: pointer;
	}
	pre {
		font-size: 11px;
		overflow-x: auto;
		max-height: 480px;
	}
	.small {
		font-size: 12px;
	}
</style>
