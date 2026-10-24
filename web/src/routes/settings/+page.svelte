<script lang="ts">
	import { goto } from '$app/navigation';
	import { onMount } from 'svelte';
	import { api, ApiError, qs } from '#lib/api.ts';
	import { ago, bytes, count, dateTime, label } from '#lib/format.ts';
	import type { Notification, SavedSearch } from '#lib/types.ts';

	type Config = Record<string, any>;
	interface Settings {
		config: Config;
		screenshots: { count: number; bytes: number };
	}
	interface Health {
		status: string;
		version: string;
		database: string;
		sync?: string;
		render?: string;
		ct?: string;
		render_queue?: Record<string, number>;
	}

	let settings = $state<Settings | null>(null);
	let health = $state<Health | null>(null);
	let saved = $state<SavedSearch[]>([]);
	let notice = $state('');
	let editing = $state('');
	let editName = $state('');
	let deleting = $state('');
	let notifications = $state<Notification[]>([]);
	let channels = $state<string[]>([]);

	async function loadNotifications() {
		const r = await api.get<{ notifications: Notification[]; channels: string[] }>('/api/notifications');
		notifications = r.notifications;
		channels = r.channels;
	}

	async function loadSaved() {
		saved = (await api.get<{ saved_searches: SavedSearch[] }>('/api/saved-searches')).saved_searches;
	}

	async function loadHealth() {
		try {
			health = await (await fetch('/api/health')).json();
		} catch {
			health = null;
		}
	}

	onMount(() => {
		api.get<Settings>('/api/settings').then((s) => (settings = s));
		loadHealth();
		loadSaved();
		loadNotifications();
		const t = setInterval(() => {
			loadHealth();
			loadNotifications();
		}, 30_000);
		return () => clearInterval(t);
	});

	async function patch(s: SavedSearch, body: Record<string, unknown>) {
		notice = '';
		try {
			await api.patch(`/api/saved-searches/${s.id}`, body);
			await loadSaved();
			return true;
		} catch (e) {
			notice = e instanceof ApiError ? e.message : String(e);
			return false;
		}
	}

	async function rename(e: SubmitEvent, s: SavedSearch) {
		e.preventDefault();
		const name = editName.trim();
		if (!name || name === s.name || (await patch(s, { name }))) editing = '';
	}

	async function remove(s: SavedSearch) {
		notice = '';
		try {
			await api.del(`/api/saved-searches/${s.id}`);
			deleting = '';
			await loadSaved();
		} catch (e) {
			notice = e instanceof ApiError ? e.message : String(e);
		}
	}

	async function logout() {
		await api.del('/api/session');
		await goto('/login');
	}

	/** "504h0m0s" as "21 days": Go prints durations in hours at most. */
	function duration(v: string): string {
		const m = /^(?:(\d+)h)?(?:(\d+)m(?!s))?(?:([\d.]+)s)?$/.exec(v);
		if (!m) return v;
		const s = Number(m[1] ?? 0) * 3600 + Number(m[2] ?? 0) * 60 + Number(m[3] ?? 0);
		const unit = (n: number, u: string) => `${n} ${u}${n === 1 ? '' : 's'}`;
		if (s >= 2 * 86400 && s % 86400 === 0) return unit(s / 86400, 'day');
		if (s >= 3600 && s % 3600 === 0) return unit(s / 3600, 'hour');
		if (s >= 60 && s % 60 === 0) return unit(s / 60, 'minute');
		return unit(s, 'second');
	}

	type Kind = 'text' | 'mono' | 'duration' | 'secret' | 'flag' | 'ports' | 'origins' | 'share';
	interface Row {
		key: string;
		label: string;
		kind: Kind;
		hint?: string;
	}
	interface Group {
		title: string;
		rows: Row[];
	}

	// The config by what it governs, each key with what it means.
	const groups: Group[] = [
		{
			title: 'Instance',
			rows: [
				{ key: 'public_url', label: 'Public URL', kind: 'mono', hint: 'how runs and the browser reach Recon' },
				{ key: 'http.addr', label: 'Listen address', kind: 'mono' },
				{ key: 'api_key', label: 'API key', kind: 'secret' },
				{ key: 'database.url', label: 'Database', kind: 'secret' },
				{ key: 'log.level', label: 'Log level', kind: 'text' },
				{ key: 'log.format', label: 'Log format', kind: 'text' },
				{ key: 'geoip_dir', label: 'GeoIP files', kind: 'mono', hint: 'ASN and country' }
			]
		},
		{
			title: 'Scope sync',
			rows: [
				{ key: 'scopes_extractor.url', label: 'ScopesExtractor', kind: 'mono' },
				{ key: 'scopes_extractor.api_key', label: 'API key', kind: 'secret' },
				{ key: 'scopes_extractor.interval', label: 'Every', kind: 'duration' },
				{ key: 'scopes_extractor.timeout', label: 'Timeout', kind: 'duration' },
				{ key: 'scopes_extractor.max_removed_ratio', label: 'Refused above', kind: 'text', hint: 'share of programs one pass may archive' }
			]
		},
		{
			title: 'Discovery',
			rows: [
				{ key: 'discovery.auto_scan', label: 'Auto-scan default', kind: 'text', hint: 'for programs that set none' },
				{ key: 'discovery.ports', label: 'Ports', kind: 'ports' },
				{ key: 'fastrecon.url', label: 'FastRecon', kind: 'mono' },
				{ key: 'fastrecon.api_token', label: 'API token', kind: 'secret' },
				{ key: 'fastrecon.slots', label: 'Runs in flight', kind: 'text' },
				{ key: 'fastrecon.timeout', label: 'Run timeout', kind: 'duration' }
			]
		},
		{
			title: 'Verification',
			rows: [
				{ key: 'verification.enabled', label: 'Enabled', kind: 'flag', hint: 'probes every in-scope host on its due date' },
				{ key: 'verification.resolve', label: 'Resolve every', kind: 'duration' },
				{ key: 'verification.full', label: 'Full probe every', kind: 'duration' },
				{ key: 'verification.dormant', label: 'Dormant hosts every', kind: 'duration' },
				{ key: 'verification.archive_after', label: 'Archived after', kind: 'duration' },
				{ key: 'verification.batch', label: 'Hosts per run', kind: 'text' },
				{ key: 'verification.dead_origins', label: 'Dead origins', kind: 'origins' }
			]
		},
		{
			title: 'Renders',
			rows: [
				{ key: 'render.url', label: 'Fingerprinter', kind: 'mono' },
				{ key: 'render.concurrency', label: 'Most in flight', kind: 'text' },
				{ key: 'render.periodic', label: 'Again after', kind: 'duration' },
				{ key: 'render.retry', label: 'Retry after', kind: 'duration' },
				{ key: 'render.timeout', label: 'Timeout', kind: 'duration' },
				{ key: 'screenshots.dir', label: 'Screenshots', kind: 'mono' },
				{ key: 'screenshots.keep', label: 'Kept per asset', kind: 'text' },
				{ key: 'screenshots.quality', label: 'JPEG quality', kind: 'text' },
				{ key: 'screenshots.disk_threshold', label: 'Disk alert past', kind: 'share', hint: 'of the filesystem in use' }
			]
		},
		{
			title: 'Certificate Transparency',
			rows: [
				{ key: 'ct.url', label: 'Feed', kind: 'mono', hint: 'names under a wildcard are probed at once, verification on or off' },
				{ key: 'ct.max_per_hour', label: 'Per program and hour', kind: 'text', hint: 'names created at most, the rest counted' },
				{ key: 'ct.reload', label: 'Wildcards read every', kind: 'duration' }
			]
		},
		{
			title: 'Notifications',
			rows: [
				{ key: 'notify.discord_url', label: 'Discord webhook', kind: 'secret' },
				{ key: 'notify.webhook_url', label: 'Webhook', kind: 'secret' },
				{ key: 'notify.webhook_header', label: 'Webhook header', kind: 'secret' },
				{ key: 'notify.broken_after', label: 'Broken after', kind: 'duration', hint: 'a component failing this long is announced' }
			]
		}
	];

	// Every leaf of the config, a secret counting as one: what no group names
	// still shows, under Other, so a new setting is never hidden.
	function leaves(o: Config, prefix = ''): string[] {
		return Object.entries(o).flatMap(([k, v]) => {
			const key = prefix ? `${prefix}.${k}` : k;
			const nested = v && typeof v === 'object' && !Array.isArray(v) && !('set' in v);
			return nested ? leaves(v, key) : [key];
		});
	}
	const shownGroups = $derived.by((): Group[] => {
		if (!settings) return groups;
		const named = new Set(groups.flatMap((g) => g.rows.map((r) => r.key)));
		const rest = leaves(settings.config).filter((k) => !named.has(k));
		if (!rest.length) return groups;
		const kind = (k: string): Kind => {
			const v = get(settings!.config, k);
			if (v && typeof v === 'object' && 'set' in v) return 'secret';
			if (typeof v === 'boolean') return 'flag';
			return 'mono';
		};
		return [...groups, { title: 'Other', rows: rest.map((k) => ({ key: k, label: k.split('.').pop()!, kind: kind(k) })) }];
	});

	const get = (c: Config, key: string) => key.split('.').reduce<any>((o, k) => o?.[k], c);
	const queued = $derived(health?.render_queue ? Object.values(health.render_queue).reduce((a, b) => a + b, 0) : null);

	function tone(v: string | undefined): string {
		if (v === 'ok') return 'ok';
		if (v === 'failing' || v === 'unreachable' || v === 'degraded') return 'bad';
		if (v === 'pending') return 'info';
		return '';
	}
</script>

<svelte:head><title>Settings · Recon</title></svelte:head>

<div class="page">
	<div class="page-head">
		<div>
			<h1>Settings</h1>
			<p>Saved searches are yours to edit. The rest is the config file, read only, as it is in effect.</p>
		</div>
		<button class="btn" type="button" onclick={logout}>Sign out</button>
	</div>
	{#if notice}<p class="error" role="alert">{notice}</p>{/if}

	<section aria-label="Health" class="tiles">
		{#if health}
			{@const verification = settings ? get(settings.config, 'verification.enabled') : undefined}
			<div class="tile">
				<span class="label">Database</span>
				<span class="state {tone(health.database)}"><span class="dot" aria-hidden="true"></span>{health.database}</span>
			</div>
			<div class="tile">
				<span class="label">Scope sync</span>
				<span class="state {tone(health.sync)}"><span class="dot" aria-hidden="true"></span>{health.sync ?? '—'}</span>
			</div>
			<div class="tile">
				<span class="label">Fingerprinter</span>
				<span class="state {tone(health.render)}"><span class="dot" aria-hidden="true"></span>{health.render ?? '—'}</span>
			</div>
			<div class="tile">
				<span class="label">CT feed</span>
				<span class="state {health.ct === 'disconnected' ? 'bad' : tone(health.ct)}"><span class="dot" aria-hidden="true"></span>{health.ct ?? '—'}</span>
			</div>
			<div class="tile">
				<span class="label">Verification</span>
				<span class="state {verification ? 'ok' : ''}"><span class="dot" aria-hidden="true"></span>{verification === undefined ? '—' : verification ? 'on' : 'off'}</span>
			</div>
			<div class="tile">
				<span class="label">Render queue</span>
				<span class="big num">{queued ?? '—'}</span>
				{#if health.render_queue}
					<span class="sub num">high {health.render_queue.high} · normal {health.render_queue.normal} · low {health.render_queue.low}</span>
				{/if}
			</div>
			<div class="tile">
				<span class="label">Screenshots</span>
				<span class="big num">{settings ? bytes(settings.screenshots.bytes) : '—'}</span>
				{#if settings}<span class="sub num">{count(settings.screenshots.count)} distinct, thumbnails not counted</span>{/if}
			</div>
		{/if}
	</section>

	<section class="panel" aria-label="Saved searches">
		<h2>Saved searches</h2>
		{#if saved.length}
			<ul class="saved">
				{#each saved as s (s.id)}
					<li>
						{#if editing === s.id}
							<form class="rename" onsubmit={(e) => rename(e, s)}>
								<label class="sr-only" for="name-{s.id}">Name</label>
								<input id="name-{s.id}" class="input" bind:value={editName} required {@attach (el) => { el.focus(); el.select(); }} />
								<button class="btn btn-sm btn-primary" type="submit">Save</button>
								<button class="btn btn-sm" type="button" onclick={() => (editing = '')}>Cancel</button>
							</form>
						{:else}
							<div class="what">
								<a href={'/' + qs({ q: s.query, view: s.view === 'services' ? '' : s.view })}>{s.name}</a>
								<span class="mono muted q">{s.query || 'everything'}{s.view === 'hosts' ? ' · hosts' : ''}</span>
							</div>
						{/if}
						<label class="toggle" title="Alerts are sent from phase 7 on">
							<input type="checkbox" checked={s.alert} onchange={(e) => patch(s, { alert: e.currentTarget.checked })} /> Alert
						</label>
						{#if deleting === s.id}
							<span class="confirm">
								Delete it?
								<button class="btn btn-sm danger" type="button" onclick={() => remove(s)}>Delete</button>
								<button class="btn btn-sm" type="button" onclick={() => (deleting = '')}>Keep</button>
							</span>
						{:else if editing !== s.id}
							<span class="actions">
								<button class="btn btn-sm" type="button" onclick={() => { editing = s.id; editName = s.name; deleting = ''; }}>Rename</button>
								<button class="btn btn-sm" type="button" onclick={() => { deleting = s.id; editing = ''; }}>Delete</button>
							</span>
						{/if}
					</li>
				{/each}
			</ul>
		{:else}
			<p class="muted small">None yet. Save one from the search screen.</p>
		{/if}
	</section>

	<section class="panel" aria-label="Notifications">
		<div class="panel-head">
			<h2>Notifications</h2>
			<span class="muted small">
				{channels.length ? 'Sent to ' + channels.join(' and ') : 'No channel set: kept here only'}
			</span>
		</div>
		{#if notifications.length}
			<ul class="notes">
				{#each notifications as n (n.id)}
					<li>
						<time class="muted num" datetime={n.created_at} title={dateTime(n.created_at)}>{ago(n.created_at)}</time>
						<div class="what">
							<span>{n.kind === 'saved_search' ? `Saved search “${n.title}”: ${n.total} new match${n.total === 1 ? '' : 'es'}` : n.kind === 'ct_live' ? `${n.title}: ${n.total} name${n.total === 1 ? '' : 's'} from certificates now serving HTTP` : n.title}</span>
							{#if n.items.length}
								<span class="mono muted q">{n.items.slice(0, 3).join(' · ')}{n.total > 3 ? ` · and ${n.total - 3} more` : ''}</span>
							{/if}
							{#if n.last_error}<span class="err small">{n.last_error}</span>{/if}
						</div>
						<span class="badge {n.outcome === 'sent' ? 'ok' : n.outcome === 'failed' ? 'bad' : n.outcome === 'no_channel' ? '' : 'info'}">
							{n.outcome === 'sent' ? 'sent' : n.outcome === 'failed' ? 'failed' : n.outcome === 'no_channel' ? 'not sent' : n.attempts ? 'retrying' : 'waiting'}
						</span>
						{#if n.link}<a class="small" href={n.link.replace(/^https?:\/\/[^/]+/, '')}>Open</a>{/if}
					</li>
				{/each}
			</ul>
		{:else}
			<p class="muted small">None yet. A recon that ends, an alerting saved search with new matches or a component failing for a while sends one.</p>
		{/if}
	</section>

	{#if settings}
		<div class="groups">
			{#each shownGroups as g (g.title)}
				<section class="panel" aria-label={g.title}>
					<h2>{g.title}</h2>
					<dl>
						{#each g.rows as r (r.key)}
							{@const v = get(settings.config, r.key)}
							<div class="row">
								<dt>
									<span>{r.label}</span>
									<code class="key" title="{r.key} in the config">{r.key}</code>
								</dt>
								<dd>
									{#if r.kind === 'secret'}
										<span class="badge {v?.set ? 'ok' : 'warn'}">{v?.set ? 'set' : 'not set'}</span>
									{:else if r.kind === 'flag'}
										<span class="badge {v ? 'ok' : ''}">{v ? 'on' : 'off'}</span>
									{:else if r.kind === 'share'}
										<span class="num">{Math.round(Number(v) * 100)}%</span>
									{:else if r.kind === 'duration'}
										<span class="num" title={String(v)}>{duration(String(v))}</span>
									{:else if r.kind === 'ports'}
										<details>
											<summary class="num">{(v as number[]).length} ports</summary>
											<p class="mono ports">{(v as number[]).join(' ')}</p>
										</details>
									{:else if r.kind === 'origins'}
										{#if (v as unknown[]).length}
											<ul class="origins">
												{#each v as Record<string, any>[] as o, i (i)}
													<li>
														<span>{o.name}</span>
														<span class="muted small mono">{[o.server && `server ${o.server}`, o.title && `title ${o.title}`, o.statuses?.length && o.statuses.join(', ')].filter(Boolean).join(' · ')}</span>
													</li>
												{/each}
											</ul>
										{:else}—{/if}
									{:else if v === '' || v === null || v === undefined}
										<span class="muted">not set</span>
									{:else}
										<span class:mono={r.kind === 'mono'}>{r.kind === 'text' ? label(String(v)) : typeof v === 'object' ? JSON.stringify(v) : String(v)}</span>
									{/if}
									{#if r.hint}<span class="hint">{r.hint}</span>{/if}
								</dd>
							</div>
						{/each}
					</dl>
				</section>
			{/each}
		</div>
		<p class="muted small foot">Version {health?.version ?? '—'}. Change a value in <code class="mono">config.yml</code> or a <code class="mono">RECON_*</code> variable, then restart.</p>
	{/if}
</div>

<style>
	.tiles {
		display: grid;
		grid-template-columns: repeat(auto-fill, minmax(190px, 1fr));
		gap: 12px;
	}
	.tile {
		display: flex;
		flex-direction: column;
		gap: 6px;
		padding: 14px 16px;
		border-radius: 12px;
		background: var(--surface);
		box-shadow: 0 0 0 1px oklch(1 0 0 / 0.06);
		min-width: 0;
	}
	.state {
		display: inline-flex;
		align-items: center;
		gap: 8px;
		font-size: 15px;
		font-weight: 500;
		color: var(--text-body);
	}
	.state .dot {
		width: 8px;
		height: 8px;
		color: var(--faint);
	}
	.state.ok .dot {
		color: var(--accent);
	}
	.state.bad {
		color: oklch(0.85 0.1 25);
	}
	.state.bad .dot {
		color: var(--bad);
	}
	.state.info .dot {
		color: var(--info);
	}
	.big {
		font-size: 22px;
		font-weight: 600;
		line-height: 1.1;
		color: var(--text-strong);
	}
	.sub {
		font-size: 12px;
		color: var(--faint);
	}
	.saved {
		list-style: none;
		margin: 0;
		padding: 0;
		display: flex;
		flex-direction: column;
	}
	.saved li {
		display: flex;
		gap: 12px;
		align-items: center;
		padding: 10px 0;
		border-top: 1px solid var(--line-soft);
		flex-wrap: wrap;
	}
	.saved li:first-child {
		border-top: 0;
		padding-top: 0;
	}
	.what {
		flex: 1 1 280px;
		min-width: 0;
		display: flex;
		flex-direction: column;
		gap: 2px;
	}
	.what a {
		font-weight: 500;
		text-decoration: none;
	}
	.q {
		font-size: 12px;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.rename {
		flex: 1 1 280px;
		display: flex;
		gap: 8px;
	}
	.rename .input {
		flex: 1 1 auto;
	}
	.toggle {
		font-size: 12px;
		color: var(--muted);
		display: flex;
		gap: 6px;
		align-items: center;
	}
	.actions,
	.confirm {
		display: flex;
		gap: 6px;
		align-items: center;
		font-size: 13px;
	}
	.danger {
		background: var(--bad-bg);
		border-color: transparent;
		color: oklch(0.9 0.06 25);
	}
	.panel-head {
		display: flex;
		justify-content: space-between;
		align-items: baseline;
		gap: 12px;
		flex-wrap: wrap;
	}
	.panel-head h2 {
		font-size: 11px;
		font-weight: 600;
		letter-spacing: 0.05em;
		text-transform: uppercase;
		color: var(--faint);
	}
	.notes {
		list-style: none;
		margin: 0;
		padding: 0;
		display: flex;
		flex-direction: column;
	}
	.notes li {
		display: flex;
		gap: 12px;
		align-items: center;
		padding: 9px 0;
		border-top: 1px solid var(--line-soft);
		font-size: 13px;
	}
	.notes li:first-child {
		border-top: 0;
		padding-top: 0;
	}
	.notes time {
		flex: 0 0 110px;
		font-size: 12px;
	}
	.err {
		color: oklch(0.8 0.1 25);
	}
	.groups {
		display: grid;
		grid-template-columns: repeat(auto-fill, minmax(420px, 1fr));
		gap: 20px;
		align-items: start;
	}
	dl {
		margin: 0;
		display: flex;
		flex-direction: column;
	}
	.row {
		display: grid;
		grid-template-columns: minmax(140px, 0.9fr) 1.1fr;
		gap: 4px 16px;
		padding: 9px 0;
		border-top: 1px solid var(--line-soft);
	}
	.row:first-child {
		border-top: 0;
		padding-top: 0;
	}
	dt {
		display: flex;
		flex-direction: column;
		gap: 1px;
		font-size: 13px;
		color: var(--text-body);
		min-width: 0;
	}
	.key {
		font-family: var(--mono);
		font-size: 11px;
		color: var(--faint);
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	dd {
		margin: 0;
		display: flex;
		flex-direction: column;
		gap: 2px;
		align-items: flex-start;
		font-size: 13px;
		min-width: 0;
		overflow-wrap: anywhere;
	}
	.hint {
		font-size: 12px;
		color: var(--faint);
	}
	summary {
		cursor: pointer;
	}
	.ports {
		margin: 6px 0 0;
		font-size: 12px;
		color: var(--muted);
		word-spacing: 4px;
	}
	.origins {
		list-style: none;
		margin: 0;
		padding: 0;
		display: flex;
		flex-direction: column;
		gap: 4px;
	}
	.origins li {
		display: flex;
		flex-direction: column;
	}
	.small {
		font-size: 12px;
		margin: 0;
	}
	.foot {
		margin-top: -4px;
	}
	@media (max-width: 520px) {
		.groups {
			grid-template-columns: 1fr;
		}
		.row {
			grid-template-columns: 1fr;
		}
	}
</style>
