<script lang="ts">
	import { page } from '$app/state';
	import { href, parseFilters, programHref, withoutFilter } from '$lib/query';
	import type { Program } from '$lib/types';

	interface Props {
		programs: Program[];
	}

	const { programs }: Props = $props();

	/**
	 * The perimeter the list is showing, read from the URL rather than held in state.
	 *
	 * The filter lives in the query string so that a filtered list is a shareable
	 * link, and a switcher holding its own selection would be a second source of
	 * truth: the two would disagree the first time somebody used the back button.
	 */
	const filters = $derived(parseFilters(page.url.searchParams));
	const selected = $derived(filters.find((filter) => filter.field === 'program_id')?.value ?? '');
	const current = $derived(programs.find((program) => program.id === selected));

	/** Clearing the programme keeps every other filter, which is what somebody
	 *  switching perimeters means. */
	const allHref = $derived(
		href(selected ? withoutFilter(filters, { field: 'program_id', op: 'eq', value: selected }) : filters)
	);

	let open = $state(false);
</script>

<div class="switcher">
	<button class="pill" type="button" onclick={() => (open = !open)} aria-expanded={open}>
		<span class="dot" class:suspended={current?.state === 'suspended'}></span>
		{current ? current.name : 'all programs'}
		<span class="caret">▾</span>
	</button>

	{#if open}
		<!-- A plain overlay rather than a library. The list is a handful of entries and
		     each one is a link, so the back button and a middle click both work. -->
		<button class="scrim" type="button" aria-label="Close" onclick={() => (open = false)}></button>
		<ul>
			<li>
				<a href={allHref} class:on={!selected} onclick={() => (open = false)}>
					all programs
					<em>every perimeter of this organisation</em>
				</a>
			</li>
			{#each programs as program (program.id)}
				<li>
					<a href={programHref(program.id)} class:on={program.id === selected} onclick={() => (open = false)}>
						{program.name}
						{#if program.state !== 'active'}<em>{program.state}</em>{/if}
					</a>
				</li>
			{/each}
			{#if programs.length === 0}
				<li><span class="none">No program. Create one in Programs.</span></li>
			{/if}
		</ul>
	{/if}
</div>

<style>
	.switcher {
		position: relative;
	}

	.pill {
		background: var(--ink);
		color: #fff;
		border: 0;
		border-radius: 999px;
		padding: 4px 10px 4px 9px;
		font-size: 12px;
		font-weight: 500;
		display: inline-flex;
		align-items: center;
		gap: 6px;
	}

	.pill:hover {
		filter: brightness;
	}

	.dot {
		width: 6px;
		height: 6px;
		border-radius: 50%;
		background: var(--signal);
	}

	.dot.suspended {
		background: var(--code-4xx);
	}

	.caret {
		font-size: 9px;
		opacity: 0.7;
	}

	.scrim {
		position: fixed;
		inset: 0;
		background: transparent;
		border: 0;
		z-index: 1;
		cursor: default;
	}

	ul {
		position: absolute;
		top: calc(100% + 6px);
		left: 0;
		z-index: 2;
		min-width: 240px;
		max-height: 60vh;
		overflow-y: auto;
		list-style: none;
		margin: 0;
		padding: 4px;
		background: var(--card);
		border: 1px solid var(--border);
		border-radius: var(--radius-card);
		box-shadow: 0 4px 20px rgba(20, 23, 25, 0.12);
	}

	li a,
	li .none {
		display: grid;
		gap: 1px;
		padding: 6px 8px;
		border-radius: var(--radius-control);
		font-size: 12.5px;
		color: var(--ink);
		text-decoration: none;
	}

	li a:hover {
		background: var(--canvas);
	}

	li a.on {
		background: var(--signal-bg);
	}

	li em {
		font-style: normal;
		font-size: 11px;
		color: var(--ink-3);
	}

	.none {
		color: var(--ink-3);
		font-size: 12px;
	}
</style>
