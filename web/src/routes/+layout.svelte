<script lang="ts">
	import '@fontsource-variable/inter';
	import '@fontsource-variable/jetbrains-mono';
	import '../app.css';
	import { page } from '$app/state';
	import Header from '#lib/components/Header.svelte';

	let { children } = $props();
</script>

<!-- The bar stays put: only the page under it scrolls. -->
<div class="shell">
	{#if page.url.pathname !== '/login'}
		<Header />
	{/if}
	<main>
		{@render children()}
	</main>
</div>

<style>
	.shell {
		height: 100dvh;
		display: flex;
		flex-direction: column;
	}
	main {
		flex: 1 1 auto;
		min-height: 0;
		overflow-y: auto;
		scrollbar-gutter: stable;
	}
</style>
