<script lang="ts">
	import { goto } from '$app/navigation';
	import Icon from './Icon.svelte';
	import { encodeFilter, searchHref, searchTerm, withSearch, type Filter } from '$lib/query';

	interface Props {
		filters: Filter[];
		/** Which shape the list is in, so searching does not silently refold it. */
		grouped?: boolean;
	}

	const { filters, grouped = true }: Props = $props();

	/**
	 * The box shows the search in force. Read from the filters, not kept as state: the URL's filter is
	 * the search, so a shared link, the back button and removing the toolbar chip all must move this
	 * field, which only works if it has no memory of its own.
	 */
	let term = $derived(searchTerm(filters));

	/**
	 * A GET form; the handler is an enhancement, not the mechanism. A typed value cannot be a plain
	 * link, but a browser-submitted form can be, which is why the load function accepts `q` and
	 * redirects to the filter it means: with no JavaScript this still searches and lands on a
	 * shareable URL.
	 */
	function search(event: SubmitEvent) {
		event.preventDefault();
		goto(searchHref(filters, term), { keepFocus: true });
	}

	/**
	 * The other filters travel as hidden fields: a GET form submits only its own inputs, so without
	 * them searching would clear the facets already chosen.
	 */
	const carried = $derived(withSearch(filters, '').map(encodeFilter));
</script>

<form class="search" method="GET" action="/" onsubmit={search} role="search">
	{#each carried as value (value)}
		<input type="hidden" name="f" {value} />
	{/each}
	{#if !grouped}
		<input type="hidden" name="group" value="none" />
	{/if}

	<label class="field">
		<span class="sr">Search by name</span>
		<input type="search" name="q" bind:value={term} placeholder="Search a name" autocomplete="off" spellcheck="false" />
	</label>

	<!-- Submit, not search-as-you-type: every keystroke is a query over the whole inventory, with the
	facets answering beside it. -->
	<button class="go" type="submit" aria-label="Search"><Icon name="search" /></button>
</form>

<style>
	.search {
		display: flex;
		gap: 4px;
		margin-bottom: 16px;
	}

	.field {
		flex: 1;
		min-width: 0;
	}

	.field input {
		width: 100%;
		box-sizing: border-box;
		background: var(--card);
		border: 1px solid var(--border);
		border-radius: var(--radius-control);
		padding: 5px 8px;
		font-size: 12px;
		color: var(--ink);
		font-family: var(--font-mono);
	}

	.field input:focus {
		outline: none;
		border-color: var(--signal);
		box-shadow: 0 0 0 2px var(--signal-bg);
	}

	.go {
		flex: none;
		display: grid;
		place-items: center;
		background: var(--card);
		border: 1px solid var(--border);
		border-radius: var(--radius-control);
		color: var(--ink-3);
		font-size: 12px;
		padding: 0 8px;
	}

	.go:hover {
		border-color: var(--ink-3);
		color: var(--ink);
	}

	.go :global(svg) {
		width: 14px;
		height: 14px;
	}

	.sr {
		position: absolute;
		width: 1px;
		height: 1px;
		overflow: hidden;
		clip-path: inset(50%);
		white-space: nowrap;
	}
</style>
