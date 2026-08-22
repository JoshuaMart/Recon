<script lang="ts">
	import { ago, exact } from '$lib/format';
	import { claimed, queueLines } from '$lib/queue';
	import type { PageData } from './$types';

	const { data }: { data: PageData } = $props();

	const lines = $derived(queueLines(data.queue.depths, data.programs));
	const runs = $derived(data.queue.runs);
	const names = $derived(new Map(data.programs.map((program) => [program.id, program.name])));

	/** Nothing scheduled anywhere. Said once, rather than as four rows of zeroes. */
	const quiet = $derived(lines.every((line) => line.due + line.later + line.in_run === 0));

	// Named after what each schedule does rather than after its column, except
	// where the system already has a word for it.
	//
	// "Render" was the first spelling of the fingerprint queue and it was one
	// name too many: the column is next_fingerprint_at, the observation layer is
	// fingerprint, and the service is the Fingerprinter, so a fourth word on the
	// one screen somebody reads to work out why nothing is moving is a word they
	// have to translate first. It was asked about the day it was first seen.
	//
	// "Full probe" keeps its own name for the opposite reason: the column is
	// next_full_at and "full" alone says nothing, where the queue is a resolution
	// followed by a port scan and an http probe.
	const labels: Record<string, string> = {
		fingerprint: 'Fingerprint',
		full: 'Full probe',
		resolve: 'Resolve'
	};
</script>

<svelte:head><title>Queue · recon</title></svelte:head>

<div class="queue">
	<header>
		<h1>Queue</h1>
		<span class="dim">what the next tick can dispatch, and what a run already holds</span>
	</header>

	<section class="panel">
		<h2>Checks</h2>

		{#if quiet}
			<p class="dim lead">
				Nothing is scheduled. Either nothing has been discovered yet, or every check has run and none is due.
			</p>
		{/if}

		<ul class="lines">
			{#each lines as line (line.queue)}
				<li class:empty={line.due + line.later + line.in_run === 0}>
					<div class="head">
						<span class="type">{labels[line.queue]}</span>
						<span class="spacer"></span>
						<span class="count" class:on={line.due > 0}>{line.due}<em>due</em></span>
						<span class="count">{line.later}<em>scheduled</em></span>
						<span class="count" class:flight={line.in_run > 0}>{line.in_run}<em>in flight</em></span>
					</div>

					{#if line.shares.length > 1}
						<!-- Only when several programs share the queue. On one program the
						     breakdown repeats the line above it. -->
						<ul class="shares">
							{#each line.shares as share (share.program_id)}
								<li>
									<a href="/programs/{share.program_id}">{share.name}</a>
									<span class="spacer"></span>
									<span class="mono">{share.due}</span>
									<span class="mono dim">{share.later}</span>
									<span class="mono dim">{share.in_run}</span>
								</li>
							{/each}
						</ul>
					{/if}
				</li>
			{/each}
		</ul>
	</section>

	<section class="panel">
		<h2>Recent runs</h2>

		{#if runs.length === 0}
			<p class="dim lead">No job yet. A discovery scan starts from the page of a program.</p>
		{:else}
			<table>
				<thead>
					<tr>
						<th>Kind</th>
						<th>Program</th>
						<th>State</th>
						<th class="num">Observations</th>
						<th>Opened</th>
					</tr>
				</thead>
				<tbody>
					{#each runs as job (job.id)}
						<tr>
							<td>{job.kind}</td>
							<td class="prog">{names.get(job.program_id) ?? job.program_id.slice(0, 8)}</td>
							<td>
								<span class="state {job.state}">{job.state}</span>
								{#if !claimed(job.state, job.started_at)}
									<!-- A pending run nobody claimed is not a scan to wait for.
									     The lease token went to a provisioner and came back to nobody. -->
									<span class="unclaimed">nothing has claimed it</span>
								{/if}
							</td>
							<td class="num mono">{job.observations || ''}</td>
							<td class="dim" title={exact(job.created_at)}>{ago(job.created_at)}</td>
						</tr>
						{#if job.error}
							<tr class="why">
								<td colspan="5">{job.error}</td>
							</tr>
						{/if}
					{/each}
				</tbody>
			</table>
		{/if}
	</section>
</div>

<style>
	.queue {
		padding: 14px 18px 60px;
		min-width: 0;
		display: grid;
		gap: 12px;
		align-content: start;
	}

	header {
		display: flex;
		align-items: baseline;
		gap: 9px;
	}

	h1 {
		font-size: 16px;
		margin: 0;
		font-weight: 600;
	}

	.panel {
		background: var(--card);
		border: 1px solid var(--border);
		border-radius: var(--radius-card);
		box-shadow: var(--card-shadow);
		padding: 16px 18px 18px;
	}

	h2 {
		font-size: 11px;
		text-transform: uppercase;
		letter-spacing: 0.07em;
		color: var(--ink-3);
		font-weight: 600;
		margin: 0 0 10px;
	}

	.lead {
		margin: 0 0 12px;
		font-size: 12px;
	}

	.lines,
	.shares {
		list-style: none;
		margin: 0;
		padding: 0;
	}

	.lines > li {
		padding: 9px 0;
		border-bottom: 1px solid var(--border-2);
	}

	.lines > li:last-child {
		border-bottom: 0;
	}

	.head {
		display: flex;
		align-items: baseline;
		gap: 18px;
	}

	.type {
		font-size: 12px;
		font-weight: 600;
	}

	.spacer {
		flex: 1;
	}

	.count {
		font-family: var(--font-mono);
		font-size: 15px;
		color: var(--ink-3);
		min-width: 74px;
		text-align: right;
	}

	.count em {
		display: block;
		font-family: var(--font-sans);
		font-style: normal;
		font-size: 9.5px;
		text-transform: uppercase;
		letter-spacing: 0.06em;
		color: var(--ink-3);
	}

	.count.on {
		color: var(--ink);
	}

	.count.flight {
		color: var(--signal);
	}

	.empty .type {
		color: var(--ink-3);
	}

	.shares {
		margin-top: 7px;
		padding-left: 10px;
		border-left: 2px solid var(--border-2);
	}

	.shares li {
		display: flex;
		align-items: baseline;
		gap: 14px;
		font-size: 12px;
		padding: 2px 0;
	}

	.shares a {
		color: var(--ink-2);
		text-decoration: none;
	}

	.shares a:hover {
		color: var(--signal);
	}

	.shares .mono {
		min-width: 74px;
		text-align: right;
	}

	table {
		width: 100%;
		border-collapse: collapse;
		font-size: 12px;
	}

	th {
		text-align: left;
		font-size: 10px;
		text-transform: uppercase;
		letter-spacing: 0.06em;
		color: var(--ink-3);
		font-weight: 600;
		padding: 0 10px 6px 0;
		border-bottom: 1px solid var(--border-2);
	}

	td {
		padding: 6px 10px 6px 0;
		border-bottom: 1px solid var(--border-2);
		color: var(--ink-2);
	}

	tr:last-child td {
		border-bottom: 0;
	}

	/*
	 * Right aligned so the counts line up, but not flush: this column is followed
	 * by another, and a zeroed right padding put a four digit count against the
	 * word next to it. The header did the same, and read as one word.
	 */
	.num {
		text-align: right;
		padding-right: 22px;
	}

	.prog {
		color: var(--ink);
	}

	.state {
		font-size: 10px;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		font-weight: 600;
		color: var(--ink-3);
	}

	.state.running {
		color: var(--signal);
	}

	.state.completed {
		color: var(--ink-2);
	}

	.state.failed {
		color: var(--code-5xx);
	}

	.unclaimed {
		font-size: 11px;
		color: var(--code-4xx);
		margin-left: 6px;
	}

	.why td {
		border-bottom: 0;
		padding-top: 0;
		font-family: var(--font-mono);
		font-size: 11px;
		color: var(--code-5xx);
	}

	.mono {
		font-family: var(--font-mono);
	}

	.dim {
		color: var(--ink-3);
		font-size: 12px;
	}
</style>
