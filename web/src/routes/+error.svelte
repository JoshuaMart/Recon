<script lang="ts">
	import { page } from '$app/state';

	/**
	 * A refused query carries the path of the offending clause, which is why the
	 * compiler returns a typed error at all: the message can point at the filter
	 * somebody just added rather than at the request. So it is shown as it came.
	 */
	const refused = $derived(page.status === 400);
</script>

<svelte:head><title>{page.status} · recon</title></svelte:head>

<div class="pane">
	<h1>{page.status}</h1>
	<p>{page.error?.message ?? 'Something went wrong.'}</p>

	{#if refused}
		<p class="dim">The control plane refused the query. The path in the message names the clause it stopped on.</p>
	{/if}

	<a class="btn" href="/">Back to the inventory</a>
</div>

<style>
	.pane {
		margin: 40px auto;
		max-width: 520px;
		background: var(--card);
		border: 1px solid var(--border);
		border-radius: var(--radius-card);
		box-shadow: var(--card-shadow);
		padding: 22px;
	}

	h1 {
		font-family: var(--font-mono);
		font-size: 28px;
		margin: 0 0 6px;
		color: var(--ink);
	}

	p {
		margin: 0 0 12px;
		font-size: 13px;
	}
</style>
