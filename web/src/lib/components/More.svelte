<script lang="ts">
	// Loads the next page before the bottom is reached, in whichever container
	// scrolls. The button stays for the keyboard.
	let { onmore, busy }: { onmore: () => void; busy: boolean } = $props();

	let el: HTMLElement;

	// How far below the visible area a page is already asked for.
	const ahead = 800;

	function check() {
		if (!busy && el && el.getBoundingClientRect().top < innerHeight + ahead) onmore();
	}

	$effect(() => {
		if (busy) return;
		// Once now: a page too short to scroll keeps loading.
		check();
		// Captured, so a scroll in any container reaches it.
		const opts = { capture: true, passive: true };
		addEventListener('scroll', check, opts);
		addEventListener('resize', check, opts);
		return () => {
			removeEventListener('scroll', check, opts);
			removeEventListener('resize', check, opts);
		};
	});
</script>

<div class="more" bind:this={el}>
	<button class="btn btn-sm" type="button" onclick={onmore} disabled={busy}>{busy ? 'Loading…' : 'Load more'}</button>
</div>

<style>
	.more {
		display: flex;
		justify-content: center;
		padding: 16px 0;
		/* Never the scroll anchor: kept in view by the browser as rows are
		   appended, it would load every page in a row. */
		overflow-anchor: none;
	}
</style>
