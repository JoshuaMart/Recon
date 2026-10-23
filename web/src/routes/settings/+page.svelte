<script lang="ts">
	import { goto } from '$app/navigation';
	import { onMount } from 'svelte';
	import { api, ApiError, qs } from '#lib/api.ts';
	import { bytes, count, label } from '#lib/format.ts';
	import type { SavedSearch } from '#lib/types.ts';

	interface Settings {
		config: Record<string, unknown>;
		screenshots: { count: number; bytes: number };
	}

	let settings = $state<Settings | null>(null);
	let health = $state<Record<string, unknown> | null>(null);
	let saved = $state<SavedSearch[]>([]);
	let notice = $state('');

	async function loadSaved() {
		saved = (await api.get<{ saved_searches: SavedSearch[] }>('/api/saved-searches')).saved_searches;
	}

	onMount(() => {
		api.get<Settings>('/api/settings').then((s) => (settings = s));
		fetch('/api/health').then(async (r) => (health = await r.json()));
		loadSaved();
	});

	async function rename(s: SavedSearch) {
		const name = prompt('Name', s.name)?.trim();
		if (!name || name === s.name) return;
		try {
			await api.patch(`/api/saved-searches/${s.id}`, { name });
			await loadSaved();
		} catch (e) {
			notice = e instanceof ApiError ? e.message : String(e);
		}
	}

	async function remove(s: SavedSearch) {
		if (!confirm(`Delete the saved search “${s.name}”?`)) return;
		await api.del(`/api/saved-searches/${s.id}`);
		await loadSaved();
	}

	async function logout() {
		await api.del('/api/session');
		await goto('/login');
	}

	/** The config as dotted rows: what is in effect, read only. */
	function flatten(o: Record<string, unknown>, prefix = ''): [string, string][] {
		const out: [string, string][] = [];
		for (const [k, v] of Object.entries(o)) {
			const key = prefix ? `${prefix}.${k}` : k;
			if (v && typeof v === 'object' && !Array.isArray(v)) {
				const obj = v as Record<string, unknown>;
				if (Object.keys(obj).length === 1 && 'set' in obj) out.push([key, obj.set ? 'set' : 'not set']);
				else out.push(...flatten(obj, key));
			} else if (Array.isArray(v)) {
				out.push([key, v.map((x) => (x && typeof x === 'object' ? JSON.stringify(x) : String(x))).join(', ') || '—']);
			} else out.push([key, v === '' || v === null ? '—' : String(v)]);
		}
		return out;
	}
</script>

<svelte:head><title>Settings · Recon</title></svelte:head>

<div class="page">
	<div class="page-head">
		<div>
			<h1>Settings</h1>
			<p>Saved searches live here. Everything else is the config file, shown as it is in effect.</p>
		</div>
		<button class="btn" type="button" onclick={logout}>Sign out</button>
	</div>
	{#if notice}<p class="notice">{notice}</p>{/if}

	<div class="cols">
		<div class="main">
			<section class="panel" aria-label="Saved searches">
				<h2>Saved searches</h2>
				{#if saved.length}
					<ul class="saved">
						{#each saved as s (s.id)}
							<li>
								<div>
									<a href={'/' + qs({ q: s.query, view: s.view === 'services' ? '' : s.view })}>{s.name}</a>
									<span class="mono muted small">{s.query || 'everything'}{s.view === 'hosts' ? ' · hosts' : ''}</span>
								</div>
								<label class="toggle" title="Alerts are sent from phase 7 on">
									<input
										type="checkbox"
										checked={s.alert}
										onchange={async (e) => {
											await api.patch(`/api/saved-searches/${s.id}`, { alert: e.currentTarget.checked });
											await loadSaved();
										}}
									/> Alert
								</label>
								<button class="btn btn-sm" type="button" onclick={() => rename(s)}>Rename</button>
								<button class="btn btn-sm" type="button" onclick={() => remove(s)}>Delete</button>
							</li>
						{/each}
					</ul>
				{:else}
					<p class="muted small">None yet. Save one from the search screen.</p>
				{/if}
			</section>

			{#if settings}
				<section class="panel" aria-label="Config in effect">
					<h2>Config in effect</h2>
					<dl class="kv config">
						{#each flatten(settings.config) as [k, v] (k)}<dt class="mono">{k}</dt><dd class="mono">{v}</dd>{/each}
					</dl>
				</section>
			{/if}
		</div>

		<aside class="side">
			<section class="panel" aria-label="Health">
				<h2>Health</h2>
				{#if health}
					<dl class="kv">
						{#each Object.entries(health) as [k, v] (k)}
							<dt>{label(k)}</dt>
							<dd>
								{#if v && typeof v === 'object'}
									{Object.entries(v).map(([a, b]) => `${a} ${b}`).join(' · ')}
								{:else}
									<span class:bad={v === 'failing' || v === 'unreachable' || v === 'degraded'}>{v}</span>
								{/if}
							</dd>
						{/each}
					</dl>
				{/if}
			</section>
			{#if settings}
				<section class="panel" aria-label="Storage">
					<h2>Screenshot storage</h2>
					<p class="big num">{bytes(settings.screenshots.bytes)}</p>
					<p class="muted small num">{count(settings.screenshots.count)} distinct screenshots, thumbnails not counted</p>
				</section>
			{/if}
		</aside>
	</div>
</div>

<style>
	.saved {
		list-style: none;
		margin: 0;
		padding: 0;
		display: flex;
		flex-direction: column;
		gap: 10px;
	}
	.saved li {
		display: flex;
		gap: 10px;
		align-items: center;
	}
	.saved li div {
		flex: 1 1 auto;
		min-width: 0;
		display: flex;
		flex-direction: column;
	}
	.toggle {
		font-size: 12px;
		color: var(--muted);
		display: flex;
		gap: 4px;
		align-items: center;
	}
	.small {
		font-size: 12px;
		margin: 0;
	}
	.config {
		font-size: 12px;
	}
	.big {
		font-size: 28px;
		font-weight: 600;
		margin: 0;
	}
	.bad {
		color: var(--bad);
	}
</style>
