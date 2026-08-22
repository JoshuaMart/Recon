<script lang="ts">
	import Icon from './Icon.svelte';
	import { ago, exact } from '$lib/format';
	import { keyCount, sections, type Row } from '$lib/evidence';
	import type { Evidence } from '$lib/types';

	interface Props {
		evidence: Evidence[];
		/** Layers this asset has never had an observation of, named rather than omitted. */
		never?: string[];
	}

	const { evidence, never = [] }: Props = $props();

	/**
	 * The proof, kept whole and stopped from being the first thing the page says.
	 *
	 * The raw payload is folded on a row to preserve density; on this view it was open,
	 * flat and forty screens long, which is the same mistake at a different scale. Folding
	 * it per layer and grouping it by the shape of the payload leaves every key reachable
	 * and lets the panels above carry the reading.
	 *
	 * Nothing is filtered by a list of keys worth keeping. What the fold hides is the
	 * denylist of $lib/evidence, which drops values that are the probe's own settings
	 * echoed once per row, and their counts are shown by the panels that own them.
	 */
	let open = $state<Record<string, boolean>>({});
	let needle = $state('');

	function toggle(layer: string): void {
		open[layer] = !open[layer];
	}

	const query = $derived(needle.trim().toLowerCase());

	function matching(rows: Row[]): Row[] {
		if (!query) return rows;
		return rows.filter((row) => row.key.toLowerCase().includes(query) || row.value.toLowerCase().includes(query));
	}
</script>

<section class="dv-panel" id="evidence">
	<header class="dv-head">
		<h2>Raw evidence</h2>
		<span class="spacer"></span>
		<span class="dv-src hint">the payload of the last observation, per layer</span>
		<input class="field filter" type="search" placeholder="filter keys" bind:value={needle} aria-label="Filter keys" />
	</header>

	{#each evidence as layer (layer.layer)}
		{@const shown = open[layer.layer] ?? false}
		{@const keys = keyCount(layer.layer, layer.data)}
		<div class="row" class:on={shown}>
			<button class="head" type="button" onclick={() => toggle(layer.layer)} aria-expanded={shown}>
				<span class="chev" class:down={shown}><Icon name="chevron" /></span>
				<span class="dv-layer">{layer.layer}</span>
				<span class="dv-src">{layer.source}</span>
				{#if layer.outcome !== 'ok'}<span class="dv-outcome">{layer.outcome}</span>{/if}
				<span class="dv-src count">{keys} top-level {keys === 1 ? 'key' : 'keys'}</span>
				<span class="spacer"></span>
				<span class="dv-src" title={exact(layer.observed_at)}>observed {ago(layer.observed_at)}</span>
				{#if layer.producer_version}<span class="dv-src">{layer.producer_version}</span>{/if}
				<span class="go">{shown ? 'Hide' : 'Show'}</span>
			</button>
		</div>

		{#if shown}
			<div class="body">
				{#each sections(layer.layer, layer.data) as section (section.key || 'values')}
					{@const rows = matching(section.rows)}
					{#if rows.length > 0 || section.duplicateOf}
						<div class="block">
							<div class="bh">
								<span class="name">{section.label}</span>
								<span class="n">{section.rows.length} {section.rows.length === 1 ? 'line' : 'lines'}</span>
							</div>

							{#if section.duplicateOf}
								<!-- `http-check` writes the answering hop's headers twice, in the chain
								     and again at the top level. Both are the payload, so the block is
								     marked rather than dropped: a fold that removed a key would be a
								     fold nobody could trust. -->
								<p class="dup">
									Byte for byte the headers of <b>{section.duplicateOf}</b>, the hop that answered.
								</p>
							{:else}
								<dl class="kv">
									{#each rows as row, i (row.key + i)}
										<dt style:padding-left="{row.depth * 12}px" title={row.key}>{row.key}</dt>
										<dd title={row.title ?? ''}>
											{#if row.image}
												<!-- Rendered rather than printed. In an <img>, the safe container
												     for a value a hostile target produced. -->
												<img class="thumb" src={row.image} alt="favicon" />
											{:else}
												{row.value}
											{/if}
										</dd>
									{/each}
								</dl>
							{/if}
						</div>
					{/if}
				{/each}
			</div>
		{/if}
	{/each}

	{#each never as layer (layer)}
		<div class="row">
			<div class="head absent">
				<span class="dv-layer none">{layer}</span>
				<span class="says">no observation of this layer, ever</span>
			</div>
		</div>
	{/each}

	{#if evidence.length === 0 && never.length === 0}
		<div class="body">
			<p class="dv-note">Nothing measured yet. No observation of any layer in the window read.</p>
		</div>
	{/if}
</section>

<style>
	.hint {
		flex: none;
	}

	.filter {
		width: 160px;
		flex: none;
		font-size: 11.5px;
		padding: 4px 8px;
	}

	.row + .row .head,
	.body + .row .head {
		border-top: 1px solid var(--border-2);
	}

	.head {
		display: flex;
		align-items: center;
		gap: 9px;
		width: 100%;
		padding: 9px 16px;
		background: none;
		border: 0;
		text-align: left;
		font-size: 12px;
		color: var(--ink-3);
		min-width: 0;
	}

	.head:hover {
		background: var(--canvas);
	}

	.row.on .head {
		background: var(--canvas);
	}

	.head.absent {
		font-style: italic;
	}

	.chev {
		display: inline-flex;
		color: var(--ink-3);
	}

	.chev :global(svg) {
		width: 12px;
		height: 12px;
		transition: transform 0.12s;
	}

	.chev.down :global(svg) {
		transform: rotate(90deg);
	}

	.dv-layer.none {
		border-style: dashed;
		color: var(--ink-3);
	}

	.count {
		font-family: var(--font-mono);
	}

	.go {
		font-size: 11.5px;
		color: var(--ink-3);
		text-decoration: underline;
		text-decoration-color: var(--border);
		flex: none;
	}

	.says {
		font-size: 12px;
	}

	.body {
		padding: 4px 16px 14px;
		border-top: 1px solid var(--border-2);
	}

	.block {
		margin-top: 12px;
	}

	.bh {
		display: flex;
		align-items: baseline;
		gap: 8px;
		padding: 4px 0 5px;
	}

	.bh .name {
		font-family: var(--font-mono);
		font-size: 11px;
		font-weight: 600;
		color: var(--ink);
	}

	.bh .n {
		font-size: 10.5px;
		color: var(--ink-3);
	}

	/* A block is a rule and an indent, so a header inside chain[] reads as being inside
	   it rather than as a sibling of the chain itself. */
	.kv {
		display: grid;
		grid-template-columns: 244px minmax(0, 1fr);
		gap: 3px 14px;
		align-items: baseline;
		margin: 0;
		border-left: 1px solid var(--border);
		padding-left: 12px;
	}

	.kv dt {
		font-family: var(--font-mono);
		font-size: 11px;
		color: var(--ink-3);
		overflow-wrap: anywhere;
	}

	.kv dd {
		margin: 0;
		font-family: var(--font-mono);
		font-size: 11.5px;
		color: var(--ink-2);
		overflow-wrap: anywhere;
	}

	.thumb {
		width: 16px;
		height: 16px;
		vertical-align: middle;
		image-rendering: pixelated;
	}

	.dup {
		margin: 0 0 0 12px;
		font-size: 11.5px;
		color: var(--ink-3);
		background: var(--canvas);
		border: 1px dashed var(--border);
		border-radius: var(--radius-control);
		padding: 6px 9px;
	}

	.dup b {
		color: var(--ink-2);
		font-weight: 500;
	}
</style>
