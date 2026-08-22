<script lang="ts">
	import '../app.css';
	import Icon from '$lib/components/Icon.svelte';
	import ProgramSwitcher from '$lib/components/ProgramSwitcher.svelte';
	import { page } from '$app/state';

	const { children, data } = $props();

	/** The connect screen is its own page and takes no shell. */
	const bare = $derived(page.url.pathname === '/connect');

	/**
	 * The breadcrumb, which used to read `Assets · Search` on every screen.
	 *
	 * A trail that says the same thing everywhere is a trail that says nothing, and it was
	 * at its worst on the asset view, where the one piece of context somebody wants is
	 * which asset they are looking at. The name comes from the page's own data rather than
	 * from a store: the load function already has it, and a second source would be a
	 * second thing to keep in step.
	 */
	const crumb = $derived.by(() => {
		const path = page.url.pathname;
		if (path.startsWith('/assets/')) return { section: 'Assets', leaf: page.data.detail?.asset?.key ?? 'asset' };
		if (path.startsWith('/programs/'))
			return { section: 'Programs', leaf: page.data.detail?.program?.name ?? 'program' };
		if (path === '/programs') return { section: 'Programs', leaf: 'Scope' };
		if (path === '/feed') return { section: 'Assets', leaf: 'Live feed' };
		if (path === '/queue') return { section: 'Jobs', leaf: 'Queue' };
		return { section: 'Assets', leaf: 'Search' };
	});

	/**
	 * The rail carries the whole of phase 7, and only the first entry is built.
	 * The rest are shown as pending rather than hidden, because the shape of the
	 * phase is part of what the interface says, and rather than as links, because
	 * a link to a 404 is worse than a label that admits it is not there yet.
	 */
	const nav = [
		{ href: '/', name: 'search' as const, title: 'Search', ready: true },
		{ href: '/feed', name: 'feed' as const, title: 'Live feed', ready: true },
		{ href: '/programs', name: 'programs' as const, title: 'Programs and scope', ready: true },
		{ href: '/queue', name: 'queue' as const, title: 'Queue', ready: true },
		{ href: '/settings', name: 'settings' as const, title: 'Settings, not built yet', ready: false }
	];
</script>

{#if bare}
	{@render children()}
{:else}
	<div class="shell">
		<nav class="rail">
			<div class="swatch">r</div>
			{#each nav as item (item.href)}
				{#if item.ready}
					<a
						href={item.href}
						title={item.title}
						aria-label={item.title}
						class:on={item.href === '/' ? page.url.pathname === '/' : page.url.pathname.startsWith(item.href)}
					>
						<Icon name={item.name} />
					</a>
				{:else}
					<span class="soon" title={item.title} aria-label={item.title}>
						<Icon name={item.name} />
					</span>
				{/if}
			{/each}
		</nav>

		<div class="main">
			<!-- Breadcrumb only. The search field and the scan button that a topbar
			     usually carries were removed on purpose: filtering happens in the
			     sidebar, and a scan is a decision that belongs to a program. -->
			<header class="topbar">
				<!-- The pill is the programme filter, not a label. It was dead text
				     until program_id became a searchable field, which is what made
				     "how do I filter by programme" a question with no answer. -->
				<ProgramSwitcher programs={data.programs} />
				<span class="crumb">{crumb.section} · <b>{crumb.leaf}</b></span>
				<span class="spacer"></span>
				<form method="POST" action="/disconnect">
					<button class="link" type="submit">Disconnect</button>
				</form>
			</header>

			{@render children()}
		</div>
	</div>
{/if}

<style>
	.shell {
		display: grid;
		grid-template-columns: 56px 1fr;
		min-height: 100vh;
	}

	.rail {
		background: var(--card);
		border-right: 1px solid var(--border);
		display: flex;
		flex-direction: column;
		align-items: center;
		padding: 10px 0;
		gap: 4px;
	}

	.swatch {
		width: 26px;
		height: 26px;
		border-radius: 8px;
		background: var(--signal);
		margin-bottom: 14px;
		display: grid;
		place-items: center;
		color: #fff;
		font-weight: 700;
		font-size: 12px;
	}

	.rail a {
		position: relative;
		width: 40px;
		height: 40px;
		display: grid;
		place-items: center;
		color: var(--ink-3);
		text-decoration: none;
		border-radius: 8px;
	}

	.rail a:hover {
		background: var(--canvas);
		color: var(--ink-2);
	}

	.rail a.on {
		color: var(--ink);
		background: var(--signal-bg);
	}

	.rail .soon {
		width: 40px;
		height: 40px;
		display: grid;
		place-items: center;
		color: var(--border);
		cursor: not-allowed;
	}

	.rail a.on::before {
		content: '';
		position: absolute;
		left: -8px;
		top: 10px;
		width: 2px;
		height: 20px;
		border-radius: 2px;
		background: var(--signal);
	}

	.rail :global(svg) {
		width: 17px;
		height: 17px;
	}

	.main {
		display: flex;
		flex-direction: column;
		min-width: 0;
	}

	.topbar {
		height: 52px;
		flex: none;
		background: var(--card);
		border-bottom: 1px solid var(--border);
		display: flex;
		align-items: center;
		gap: 10px;
		padding: 0 18px;
	}

	.crumb {
		color: var(--ink-3);
		font-size: 12.5px;
	}

	.crumb b {
		color: var(--ink);
		font-weight: 500;
	}
</style>
