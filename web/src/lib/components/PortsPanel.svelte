<script lang="ts">
	import Panel from './Panel.svelte';
	import { portsOf } from '$lib/observation';
	import { href } from '$lib/query';
	import type { Asset, Evidence } from '$lib/types';

	interface Props {
		asset: Asset;
		evidence?: Evidence;
	}

	const { asset, evidence }: Props = $props();

	const ports = $derived(portsOf(evidence));
	const hostHref = $derived(asset.host ? href([{ field: 'host', op: 'eq', value: asset.host }]) : '');

	/**
	 * The count of scanned ports, which the raw tree deliberately hides.
	 *
	 * `evidence.ts` drops `scanned_ports`, `closed_ports` and `filtered_ports` because a
	 * hundred numbers identical on every asset are the probe's settings echoed once per
	 * row. Their **count** is a different fact and the one a reader wants here: it
	 * separates "nothing else is open" from "nothing else was tried", and the port rule turned
	 * every open port into an asset precisely because that distinction was invisible.
	 */
	const scanned = $derived(ports.scanned);
</script>

<Panel title="Ports" {evidence} meta="never scanned">
	{#if ports.open.length === 0}
		<p class="dv-note">
			{#if evidence}
				No port answered on this address at the last scan{scanned ? `, of ${scanned} tried` : ''}.
			{:else}
				No tcp observation. The port this asset carries comes from its canonical key .
			{/if}
		</p>
	{:else}
		<div class="dv-row wide">
			{#each ports.open as port (port)}
				<span class="dv-badge plain"><span class="v">{port}/tcp</span></span>
			{/each}
			<span class="says">
				{ports.open.includes(asset.port ?? -1) ? 'open, and one of them is this asset.' : 'open on this address.'}
			</span>
			{#if scanned && scanned > ports.open.length}
				<!-- Only when there was a width to report: a service is scanned on its own
				     port alone, and "0 of the 1 scanned" says nothing about anything. -->
				<span class="sep">·</span>
				<span class="says">
					{scanned - ports.open.length} of the {scanned} scanned answered closed or filtered, which is what says the scan
					ran.
				</span>
			{/if}
		</div>
	{/if}

	{#if hostHref}
		<p class="dv-note gap">
			<!-- the port rule makes every open port an asset of its own, so the other services of
			     this host are a filter and never a second list with its own rules. -->
			<a class="link" href={hostHref}>Every service on {asset.host} →</a>
		</p>
	{/if}
</Panel>

<style>
	.wide {
		gap: 9px;
	}

	.says {
		font-size: 12px;
		color: var(--ink-3);
	}

	.sep {
		color: var(--border);
	}

	.gap {
		margin-top: 10px;
		font-size: 11.5px;
	}
</style>
