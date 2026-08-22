<script lang="ts">
	import Icon from '$lib/components/Icon.svelte';
	import { ago, exact } from '$lib/format';
	import type { Discovery, FeedTick } from '$lib/types';

	/**
	 * The live feed.
	 *
	 * An EventSource, so reconnection and Last-Event-ID are the browser's job. The
	 * server made the cursor the event id precisely so that the resume needs nothing
	 * on this side.
	 */
	let rows = $state<Discovery[]>([]);
	let overflow = $state(0);
	/** The count itself was capped, so the number above is a floor. */
	let overflowFloor = $state(false);
	let status = $state<'connecting' | 'live' | 'lost'>('connecting');
	let opened = $state('');

	/** What the list holds. Old rows are dropped rather than accumulated: a tab left
	 *  open through a first scan would otherwise grow without bound. */
	const keep = 200;

	$effect(() => {
		opened = new Date().toISOString();

		// No cursor. A feed with none starts at the present, which is what a tab
		// opening on a live feed is asking for: the assets already in the inventory
		// are what the list next door is for.
		const source = new EventSource('/feed/stream');

		source.addEventListener('open', () => {
			status = 'live';
		});

		source.addEventListener('discoveries', (event) => {
			status = 'live';
			const tick = JSON.parse((event as MessageEvent).data) as FeedTick;
			// Newest first, and the tick arrives oldest first.
			rows = [...[...tick.discoveries].reverse(), ...rows].slice(0, keep);
			overflow += tick.overflow ?? 0;
			overflowFloor = overflowFloor || (tick.overflow_at_least ?? false);
		});

		source.addEventListener('error', () => {
			// EventSource reconnects on its own, so this is a state and not a failure.
			// Saying "lost" rather than nothing is what separates a quiet feed from a
			// broken one, which is the same distinction the cards make about absent
			// data.
			status = 'lost';
		});

		return () => source.close();
	});

	function tone(scope: string): string {
		return scope === 'in_scope' ? 'in' : scope === 'unknown' ? 'unknown' : 'out';
	}
</script>

<svelte:head><title>Live feed · recon</title></svelte:head>

<div class="feed">
	<header>
		<h1>Live feed</h1>
		<span class="state {status}">
			<span class="dot"></span>
			{status === 'live' ? 'listening' : status === 'connecting' ? 'connecting' : 'reconnecting'}
		</span>
		<span class="spacer"></span>
		<span class="since" title={exact(opened)}>since {ago(opened)}</span>
	</header>

	{#if overflow > 0}
		<!-- An overflow never produces the absence of information. The count is
		     shown rather than the rows dropped in silence, and it says when it is
		     itself a floor: "500 more" where there are forty thousand would be the
		     silent truncation this refuses. -->
		<p class="overflow">
			{overflow}{overflowFloor ? ' or more' : ''} more discoveries arrived than this feed shows. They are in the inventory.
			<a href="/">Search it</a>.
		</p>
	{/if}

	{#if rows.length === 0}
		<p class="idle">
			Nothing found since this page opened. A discovery scan pushes in batches, so this stays quiet between them.
		</p>
	{/if}

	<ol>
		{#each rows as row (row.asset_id)}
			<li>
				<span class="scope {tone(row.scope_status)}" title={row.scope_status.replace('_', ' ')}></span>
				<a class="key" href="/assets/{row.asset_id}">{row.key}</a>
				<span class="kind">{row.kind}</span>
				{#if row.step}
					<span class="step" title={row.step}>
						<Icon name="lineage" />
						<span>{row.step}</span>
					</span>
				{:else if row.discovery_source}
					<span class="step">
						<Icon name="lineage" />
						<span>found by {row.discovery_source}</span>
					</span>
				{/if}
				<time title={exact(row.first_seen)}>{ago(row.first_seen)}</time>
			</li>
		{/each}
	</ol>
</div>

<style>
	.feed {
		padding: 14px 18px 60px;
		min-width: 0;
	}

	header {
		display: flex;
		align-items: center;
		gap: 10px;
		margin-bottom: 12px;
	}

	h1 {
		font-size: 15px;
		margin: 0;
		font-weight: 600;
	}

	.state {
		display: inline-flex;
		align-items: center;
		gap: 6px;
		font-size: 11px;
		text-transform: uppercase;
		letter-spacing: 0.06em;
		font-weight: 600;
		color: var(--ink-3);
	}

	.state .dot {
		width: 7px;
		height: 7px;
		border-radius: 50%;
		background: var(--dead);
	}

	.state.live .dot {
		background: var(--signal);
	}

	.state.connecting .dot {
		background: var(--code-4xx);
	}

	.state.lost .dot {
		background: var(--code-5xx);
	}

	.state.live {
		color: var(--signal);
	}

	.since {
		font-size: 11.5px;
		color: var(--ink-3);
	}

	.overflow {
		background: var(--code-4xx-bg);
		border-left: 2px solid var(--code-4xx);
		border-radius: var(--radius-control);
		padding: 6px 10px;
		font-size: 12px;
		color: #7a5210;
		margin: 0 0 10px;
	}

	.idle {
		background: var(--card);
		border: 1px dashed var(--border);
		border-radius: var(--radius-card);
		padding: 18px;
		color: var(--ink-3);
		font-size: 12.5px;
		margin: 0;
	}

	ol {
		list-style: none;
		margin: 0;
		padding: 0;
	}

	li {
		display: flex;
		align-items: center;
		gap: 10px;
		background: var(--card);
		border: 1px solid var(--border);
		border-radius: var(--radius-card);
		box-shadow: var(--card-shadow);
		padding: 8px 12px;
		margin-bottom: 6px;
		min-width: 0;
	}

	.scope {
		width: 7px;
		height: 7px;
		border-radius: 50%;
		flex: none;
		background: var(--signal);
	}

	.scope.unknown {
		background: transparent;
		border: 2px solid var(--ink-3);
		width: 8px;
		height: 8px;
	}

	.scope.out {
		background: var(--dead);
	}

	.key {
		font-family: var(--font-mono);
		font-size: 13px;
		color: var(--ink);
		text-decoration: none;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
		flex: none;
		max-width: 40%;
	}

	.key:hover {
		text-decoration: underline;
		text-decoration-color: var(--signal);
	}

	.kind {
		font-size: 10.5px;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: var(--ink-3);
		background: var(--canvas);
		border: 1px solid var(--border);
		border-radius: var(--radius-control);
		padding: 1px 5px;
		flex: none;
	}

	.step {
		display: inline-flex;
		align-items: center;
		gap: 5px;
		flex: 1;
		min-width: 0;
		font-size: 11.5px;
		color: var(--ink-3);
	}

	.step span {
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.step :global(svg) {
		width: 12px;
		height: 12px;
	}

	time {
		flex: none;
		font-size: 11.5px;
		color: var(--ink-3);
	}
</style>
