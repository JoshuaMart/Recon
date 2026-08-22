<script lang="ts">
	import { ago, exact } from '$lib/format';
	import type { Evidence } from '$lib/types';
	import type { Snippet } from 'svelte';

	interface Props {
		title: string;
		/** The observation this panel reads, when it reads one. */
		evidence?: Evidence;
		/** Anything to say on the right when there is no observation behind the panel. */
		meta?: string;
		children: Snippet;
	}

	const { title, evidence, meta, children }: Props = $props();

	/**
	 * Two instants and not one, which is the fix for a confusion the old view carried.
	 *
	 * The identity line said "probed 1 h ago" and the evidence beneath it said "3 h ago"
	 * for the same layer. Both were true: one is `last_checked_at`, the other is when the
	 * state now held **began**. Side by side and unnamed they read as stale data, so the
	 * panel says which is which.
	 */
	const when = $derived.by(() => {
		if (!evidence) return meta ?? '';
		const observed = 'observed ' + ago(evidence.observed_at);
		if (!evidence.last_confirmed_at || evidence.last_confirmed_at === evidence.observed_at) return observed;
		return observed + ' · confirmed ' + ago(evidence.last_confirmed_at);
	});
</script>

<section class="dv-panel">
	<header class="dv-head">
		<h2>{title}</h2>
		{#if evidence}
			<span class="dv-layer">{evidence.layer}</span>
			<span class="dv-src">{evidence.source}</span>
			{#if evidence.outcome !== 'ok'}<span class="dv-outcome">{evidence.outcome}</span>{/if}
		{/if}
		<span class="spacer"></span>
		<span class="dv-src" title={evidence ? exact(evidence.observed_at) : ''}>{when}</span>
		{#if evidence?.producer_version}
			<!-- An observation is a measurement by a dated instrument, and the version
			     is what separates a real change from a revelation. -->
			<span class="dv-src" title="version of the producer that measured this">{evidence.producer_version}</span>
		{/if}
	</header>

	<div class="dv-body">
		{@render children()}
	</div>
</section>
