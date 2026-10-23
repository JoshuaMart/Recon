<script lang="ts">
	import '@fontsource-variable/inter';
	import '@fontsource-variable/jetbrains-mono';
	import '../app.css';
	import { page } from '$app/state';
	import Header from '#lib/components/Header.svelte';

	let { children } = $props();
</script>

{#if page.url.pathname !== '/login'}
	<Header />
{/if}
<main>
	{@render children()}
</main>
