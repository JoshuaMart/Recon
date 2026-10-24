<script lang="ts">
	import { onMount } from 'svelte';
	import { api } from '#lib/api.ts';
	import { ago, count, duration, label } from '#lib/format.ts';

	interface ActiveRun {
		id: string;
		kind: string;
		stage: string;
		state: string;
		runner: string;
		trigger: string;
		queued_at: string;
		started_at: string | null;
		targets: number;
		apex: string | null;
		program: string;
	}
	interface Problem {
		component: string;
		failing_since: string;
		announced: boolean;
		detail: string | null;
	}
	interface ActivityData {
		runs: ActiveRun[];
		queued: number;
		renders: { state: string; in_flight?: number; limit?: number; queue?: Record<string, number> };
		ct: {
			state: string;
			session?: { connected_at: string; disconnected_at: string | null; certificates: number; matched: number; created: number; revived: number; dropped: number };
		};
		sync: { state: string; last_success_at?: string | null; last_error?: string | null; last_error_at?: string | null };
		problems: Problem[];
	}

	let { onclose }: { onclose: () => void } = $props();
	let data = $state<ActivityData | null>(null);
	let error = $state('');

	async function load() {
		try {
			data = await api.get<ActivityData>('/api/activity');
			error = '';
		} catch (e) {
			error = String(e);
		}
	}

	onMount(() => {
		load();
		const t = setInterval(load, 5000);
		return () => clearInterval(t);
	});

	const running = $derived(data?.runs.filter((r) => r.state === 'running') ?? []);
	const waiting = $derived(data?.runs.filter((r) => r.state === 'queued') ?? []);
	const queue = $derived(data?.renders.queue ? Object.values(data.renders.queue).reduce((a, b) => a + b, 0) : 0);

	function what(r: ActiveRun): string {
		if (r.kind === 'recon') return `Recon *.${r.apex ?? '?'}`;
		const n = `${count(r.targets)} host${r.targets === 1 ? '' : 's'}`;
		return r.kind === 'candidate' ? `CT candidates, ${n}` : `Verification, ${n}`;
	}
	const tone = (s: string | undefined) => (s === 'ok' ? 'ok' : s === 'disabled' || s === 'pending' ? '' : 'bad');
</script>

<svelte:window onkeydown={(e) => e.key === 'Escape' && onclose()} />

<div class="panel activity" role="dialog" aria-label="Activity">
	<div class="head">
		<h2>Activity</h2>
		<span class="muted small">refreshed every 5 seconds</span>
	</div>
	{#if error}<p class="error small">{error}</p>{/if}
	{#if !data}
		<p class="muted small">Loading…</p>
	{:else}
		{#if data.problems.length}
			<section aria-label="Problems">
				<h3 class="label">Problems</h3>
				<ul>
					{#each data.problems as p (p.component)}
						<li class="problem">
							<span class="state bad"><span class="dot" aria-hidden="true"></span>{p.component}</span>
							<span class="muted small">failing for {ago(p.failing_since).replace(' ago', '')}{p.announced ? ', announced' : ''}</span>
							{#if p.detail}<span class="mono detail">{p.detail}</span>{/if}
						</li>
					{/each}
				</ul>
			</section>
		{/if}

		<section aria-label="Runs">
			<h3 class="label">
				Runs{#if data.runs.length}<span class="num muted"> · {running.length} running{data.queued ? `, ${data.queued} queued` : ''}</span>{/if}
			</h3>
			{#if data.runs.length}
				<ul>
					{#each [...running, ...waiting.slice(0, 5)] as r (r.id)}
						<li>
							<a href="/runs/{r.id}" onclick={onclose}>
								<span class="dot {r.state === 'running' ? 'live' : ''}" aria-hidden="true"></span>
								<span class="what">{what(r)}</span>
								<span class="muted small">{r.program}</span>
							</a>
							<span class="meta muted small num">
								{#if r.state === 'running'}
									{r.stage} on {r.runner}, {duration(r.started_at, null)}
								{:else}
									queued {ago(r.queued_at)}
								{/if}
							</span>
						</li>
					{/each}
				</ul>
			{:else}
				<p class="muted small">Nothing running. A scan from the wildcards page shows up here.</p>
			{/if}
		</section>

		<section aria-label="Renders">
			<h3 class="label">Renders</h3>
			{#if data.renders.state === 'disabled'}
				<p class="muted small">No Fingerprinter configured.</p>
			{:else}
				<p class="small">
					<span class="state {tone(data.renders.state)}"><span class="dot" aria-hidden="true"></span>{data.renders.state === 'ok' ? 'Fingerprinter up' : 'Fingerprinter unreachable'}</span>
					<span class="num">· {data.renders.in_flight ?? 0} in flight of {data.renders.limit ?? 0}</span>
					<span class="num">· {count(queue)} due</span>
					{#if queue && data.renders.queue}
						<span class="muted num">(high {data.renders.queue.high}, normal {data.renders.queue.normal}, low {data.renders.queue.low})</span>
					{/if}
				</p>
			{/if}
		</section>

		<section aria-label="CT feed">
			<h3 class="label">CT feed</h3>
			{#if data.ct.state === 'disabled'}
				<p class="muted small">Off.</p>
			{:else}
				<p class="small">
					<span class="state {tone(data.ct.state)}"><span class="dot" aria-hidden="true"></span>{label(data.ct.state === 'ok' ? 'connected' : data.ct.state)}</span>
					{#if data.ct.session}
						<span class="num">· since {ago(data.ct.session.connected_at).replace(' ago', '')}: {count(data.ct.session.certificates)} certificates, {count(data.ct.session.matched)} matched, {count(data.ct.session.created + data.ct.session.revived)} new</span>
						{#if data.ct.session.dropped}<span class="badge warn">{count(data.ct.session.dropped)} dropped</span>{/if}
					{/if}
				</p>
			{/if}
		</section>

		<section aria-label="Scope sync">
			<h3 class="label">Scope sync</h3>
			{#if data.sync.state === 'disabled'}
				<p class="muted small">No ScopesExtractor configured: manual programs only.</p>
			{:else}
				<p class="small">
					<span class="state {tone(data.sync.state)}"><span class="dot" aria-hidden="true"></span>{data.sync.state === 'ok' ? 'in sync' : label(data.sync.state)}</span>
					<span class="muted">· last success {ago(data.sync.last_success_at)}</span>
				</p>
				{#if data.sync.last_error && !data.problems.some((p) => p.detail === data?.sync.last_error)}
					<p class="mono detail">{data.sync.last_error}</p>
				{/if}
			{/if}
		</section>

		<div class="foot">
			<a href="/runs" onclick={onclose}>All runs</a>
			<a href="/settings" onclick={onclose}>Settings</a>
		</div>
	{/if}
</div>

<style>
	.activity {
		position: absolute;
		right: 0;
		top: calc(100% + 8px);
		z-index: 40;
		width: min(460px, calc(100vw - 32px));
		max-height: calc(100dvh - 90px);
		overflow-y: auto;
		box-shadow: var(--shadow);
		gap: 14px;
	}
	.head {
		display: flex;
		justify-content: space-between;
		align-items: baseline;
	}
	h2 {
		font-size: 14px;
		font-weight: 600;
	}
	h3 {
		margin: 0 0 6px;
	}
	section {
		display: flex;
		flex-direction: column;
	}
	ul {
		list-style: none;
		margin: 0;
		padding: 0;
		display: flex;
		flex-direction: column;
		gap: 6px;
	}
	li {
		display: flex;
		flex-direction: column;
		gap: 1px;
		font-size: 13px;
	}
	li a {
		display: flex;
		align-items: center;
		gap: 8px;
		color: var(--text-strong);
		text-decoration: none;
		min-width: 0;
	}
	li a .what {
		font-weight: 500;
	}
	.meta {
		padding-left: 14px;
	}
	.dot {
		width: 6px;
		height: 6px;
		color: var(--faint);
	}
	.dot.live {
		color: var(--info);
	}
	.state {
		display: inline-flex;
		align-items: center;
		gap: 6px;
		font-weight: 500;
	}
	.state .dot {
		color: var(--faint);
	}
	.state.ok .dot {
		color: var(--accent);
	}
	.state.bad {
		color: oklch(0.85 0.1 25);
	}
	.state.bad .dot {
		color: var(--bad);
	}
	.problem .detail,
	.detail {
		font-size: 11px;
		color: var(--muted);
		overflow-wrap: anywhere;
		margin: 2px 0 0;
	}
	.small {
		font-size: 12px;
		margin: 0;
	}
	p.small {
		display: flex;
		flex-wrap: wrap;
		gap: 4px 6px;
		align-items: center;
	}
	.foot {
		display: flex;
		gap: 16px;
		font-size: 12px;
		border-top: 1px solid var(--line-soft);
		padding-top: 10px;
	}
	@media (hover: hover) {
		li a:hover .what {
			color: var(--accent-text);
		}
	}
</style>
