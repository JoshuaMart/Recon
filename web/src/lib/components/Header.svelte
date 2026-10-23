<script lang="ts">
	import { page } from '$app/state';
	import { onMount } from 'svelte';
	import Logo from './Logo.svelte';

	interface Health {
		status: string;
		sync?: string;
		render?: string;
		render_queue?: Record<string, number>;
	}

	const links = [
		{ href: '/', label: 'Search', match: (p: string) => p === '/' || p.startsWith('/assets') },
		{ href: '/changes', label: 'Changes' },
		{ href: '/programs', label: 'Programs' },
		{ href: '/wildcards', label: 'Wildcards' },
		{ href: '/runs', label: 'Runs' },
		{ href: '/settings', label: 'Settings' }
	];

	let health = $state<Health | null>(null);

	async function load() {
		try {
			health = await (await fetch('/api/health')).json();
		} catch {
			health = { status: 'unreachable' };
		}
	}

	onMount(() => {
		load();
		const t = setInterval(load, 30_000);
		return () => clearInterval(t);
	});

	const current = (l: (typeof links)[number]) =>
		l.match ? l.match(page.url.pathname) : page.url.pathname.startsWith(l.href);

	const queued = $derived(
		health?.render_queue ? Object.values(health.render_queue).reduce((a, b) => a + b, 0) : null
	);
	const summary = $derived.by(() => {
		if (!health) return '';
		if (health.status === 'unreachable') return 'Recon unreachable';
		const down = [];
		if (health.sync === 'failing') down.push('sync failing');
		if (health.render === 'unreachable') down.push('Fingerprinter unreachable');
		return down.length ? down.join(' · ') : 'All services up';
	});
</script>

<header>
	<div class="bar">
		<a class="brand" href="/"><Logo /> Recon</a>
		<nav aria-label="Main">
			{#each links as l (l.href)}
				<a class="nav" href={l.href} aria-current={current(l) ? 'page' : undefined}>{l.label}</a>
			{/each}
		</nav>
		{#if health}
			<a class="health" href="/settings" class:degraded={health.status !== 'ok'}>
				<span class="dot" aria-hidden="true"></span>
				{summary}{#if queued !== null}<span class="num"> · render queue {queued}</span>{/if}
			</a>
		{/if}
	</div>
</header>

<style>
	header {
		flex: none;
		border-bottom: 1px solid var(--line);
		background: var(--bg);
	}
	.bar {
		max-width: 1440px;
		margin: 0 auto;
		padding: 0 24px;
		min-height: 56px;
		display: flex;
		align-items: center;
		gap: 24px;
		flex-wrap: wrap;
	}
	.brand {
		display: inline-flex;
		align-items: center;
		gap: 10px;
		text-decoration: none;
		color: var(--text-strong);
		font-weight: 600;
		font-size: 15px;
	}
	nav {
		display: flex;
		gap: 2px;
		flex-wrap: wrap;
		flex: 1 1 auto;
	}
	.nav {
		font-size: 13px;
		font-weight: 500;
		color: var(--muted);
		text-decoration: none;
		height: 32px;
		padding: 0 10px;
		border-radius: 6px;
		display: inline-flex;
		align-items: center;
	}
	.nav[aria-current='page'] {
		color: var(--text-strong);
		background: oklch(0.22 0.007 260);
	}
	.health {
		display: inline-flex;
		align-items: center;
		gap: 8px;
		font-size: 12px;
		color: var(--muted);
		text-decoration: none;
	}
	.health .dot {
		width: 8px;
		height: 8px;
		color: var(--accent);
	}
	.health.degraded .dot {
		color: var(--bad);
	}
	@media (max-width: 720px) {
		.bar {
			padding: 8px 16px;
			gap: 8px 16px;
		}
	}
	@media (hover: hover) {
		.nav:hover,
		.health:hover {
			color: var(--text-strong);
		}
	}
</style>
