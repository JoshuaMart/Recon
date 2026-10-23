<script lang="ts">
	import { shotURL } from '#lib/api.ts';

	let { thumb, width = 168, alt = '' }: { thumb: string | null; width?: number; alt?: string } = $props();
	let failed = $state(false);
</script>

{#if thumb && !failed}
	<img
		class="shot"
		src={shotURL(thumb)}
		{alt}
		loading="lazy"
		decoding="async"
		style:width="{width}px"
		style:height="{Math.round(width * 0.625)}px"
		onerror={() => (failed = true)}
	/>
{:else}
	<div class="none" style:width="{width}px" style:height="{Math.round(width * 0.625)}px" aria-hidden="true">
		<span>No screenshot</span>
	</div>
{/if}

<style>
	.none {
		flex: none;
		border-radius: 8px;
		background: var(--raised);
		outline: 1px solid oklch(1 0 0 / 0.06);
		outline-offset: -1px;
		display: grid;
		place-items: center;
		font-size: 11px;
		color: var(--faint);
	}
	img {
		flex: none;
	}
</style>
