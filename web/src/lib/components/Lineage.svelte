<script lang="ts">
	import type { Asset } from '$lib/types';

	interface Props {
		asset: Asset;
	}

	const { asset }: Props = $props();

	/**
	 * The chain the producer wrote, kept as it came.
	 *
	 * Each entry names a step and carries whatever else that producer recorded
	 * beside it: the run, the sources it answered from, the port that implied a
	 * service. None of it is reformatted, and that is the point of keeping it.
	 * Rewriting it into a shape of ours would lose exactly the part that answers
	 * "why is this here", which is the question a row raises the moment somebody
	 * does not recognize a name.
	 */
	const steps = $derived(asset.lineage ?? []);

	/** The detail beside a step name, flattened into one readable line. */
	function detail(step: Record<string, unknown>): string {
		return Object.entries(step)
			.filter(([key]) => key !== 'step' && key !== 'run')
			.map(([key, value]) => `${key} ${Array.isArray(value) ? value.join(', ') : String(value)}`)
			.join(' · ');
	}
</script>

<section class="lineage">
	<h2>Lineage</h2>

	{#if steps.length}
		<ol>
			{#each steps as step, i (i)}
				<li>
					<b>{step.step ?? 'unnamed step'}</b>
					{#if detail(step as unknown as Record<string, unknown>)}
						<span class="detail">{detail(step as unknown as Record<string, unknown>)}</span>
					{/if}
				</li>
			{/each}
		</ol>
	{:else if asset.discovery_source}
		<p class="one">Found by <code>{asset.discovery_source}</code>, with no recorded chain.</p>
	{:else}
		<p class="one">No recorded lineage. This asset was entered by hand rather than found.</p>
	{/if}

	{#if asset.discovery_source && steps.length}
		<p class="source">First seen by <code>{asset.discovery_source}</code>.</p>
	{/if}
</section>

<style>
	li b {
		font-weight: 500;
	}

	li .detail {
		color: var(--ink-3);
		margin-left: 6px;
	}

	.lineage {
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
		margin: 0 0 12px;
	}

	ol {
		list-style: none;
		margin: 0;
		padding: 0;
		counter-reset: step;
	}

	li {
		position: relative;
		padding: 4px 0 10px 22px;
		font-size: 12px;
		color: var(--ink-2);
		line-height: 1.5;
	}

	li::before {
		counter-increment: step;
		content: counter(step);
		position: absolute;
		left: 0;
		top: 4px;
		width: 15px;
		height: 15px;
		border-radius: 50%;
		background: var(--canvas);
		border: 1px solid var(--border);
		font-family: var(--font-mono);
		font-size: 9px;
		color: var(--ink-3);
		display: grid;
		place-items: center;
	}

	/* The chain is a descent, so the rule between steps says so. */
	li:not(:last-child)::after {
		content: '';
		position: absolute;
		left: 7px;
		top: 20px;
		bottom: 0;
		width: 1px;
		background: var(--border);
	}

	li:last-child {
		padding-bottom: 0;
		color: var(--ink);
	}

	.one,
	.source {
		margin: 0;
		font-size: 12px;
		color: var(--ink-3);
	}

	.source {
		margin-top: 12px;
		padding-top: 10px;
		border-top: 1px solid var(--border-2);
	}

	code {
		font-family: var(--font-mono);
		font-size: 11.5px;
		color: var(--ink-2);
	}
</style>
