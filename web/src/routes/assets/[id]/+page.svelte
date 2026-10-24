<script lang="ts">
	import { page } from '$app/state';
	import { api, ApiError, qs, shotURL } from '#lib/api.ts';
	import { ago, dateTime, displayKey, due, label } from '#lib/format.ts';
	import type { Program, Triage } from '#lib/types.ts';
	import { refreshUnseen } from '#lib/unseen.svelte.ts';
	import Status from '#lib/components/Status.svelte';
	import Lifecycle from '#lib/components/Lifecycle.svelte';

	interface Tech {
		name: string;
		version?: string;
		category?: string;
		cpe?: string;
		proof?: unknown;
	}
	interface Pivot {
		kind: string;
		value: string;
		others: number;
		programs: number;
		generic: boolean;
		query: string;
	}
	interface Child {
		asset_id: string;
		kind: string;
		key: string;
		lifecycle: string;
		status_code: number | null;
		title: string | null;
	}
	interface Layer {
		layer: string;
		informative_failures: number;
		non_informative_failures: number;
		first_failure_at: string | null;
		last_ok_at: string | null;
		last_checked_at: string;
	}
	interface AssetView {
		asset: {
			id: string;
			program_id: string;
			kind: string;
			key: string;
			parent_id: string | null;
			discovery_source: string;
			lineage: Record<string, unknown>[] | null;
			scope: string;
			first_seen: string;
			last_seen: string;
		};
		current: Record<string, any>;
		technologies: Tech[];
		layers: Layer[];
		pivots: Pivot[];
		children: Child[];
		triage: Triage;
	}
	interface Change {
		kind: string;
		layer: string | null;
		before: unknown;
		after: unknown;
		run_id: string | null;
		at: string;
	}
	interface Screenshot {
		id: number;
		url: string;
		thumbnail_url: string;
		width: number;
		height: number;
		taken_at: string;
	}

	const id = $derived(page.params.id ?? '');

	let data = $state<AssetView | null>(null);
	let program = $state<Program | null>(null);
	let changes = $state<Change[]>([]);
	let shots = $state<Screenshot[]>([]);
	let shown = $state(0);
	let error = $state('');
	let notice = $state('');
	let tagDraft = $state('');
	let note = $state('');
	let noteSaved = $state(true);

	$effect(() => {
		load(id);
	});

	async function load(assetID: string) {
		error = '';
		notice = '';
		data = null;
		try {
			const [a, c, s] = await Promise.all([
				api.get<AssetView>(`/api/assets/${assetID}`),
				api.get<{ changes: Change[] }>(`/api/assets/${assetID}/changes`),
				api.get<{ screenshots: Screenshot[] }>(`/api/assets/${assetID}/screenshots`)
			]);
			data = a;
			changes = c.changes;
			shots = s.screenshots;
			shown = 0;
			note = a.triage.note ?? '';
			noteSaved = true;
			api.get<Program>(`/api/programs/${a.asset.program_id}`).then((p) => (program = p));
			// Opening it is what seen means: its changes so far stop being unseen.
			triage({ seen: true }).then(refreshUnseen);
		} catch (e) {
			error = e instanceof ApiError && e.status === 404 ? 'No asset has this id.' : String(e);
		}
	}

	/** Whether the edit was stored. */
	async function triage(body: Record<string, unknown>): Promise<boolean> {
		if (!data) return false;
		try {
			data.triage = await api.patch<Triage>(`/api/assets/${id}/triage`, body);
			return true;
		} catch (e) {
			notice = e instanceof ApiError ? e.message : String(e);
			return false;
		}
	}

	async function action(kind: 'render' | 'probe') {
		notice = '';
		try {
			const r = await api.post<{ queued: boolean; reason?: string }>(`/api/assets/${id}/${kind}`);
			notice = r.queued
				? kind === 'render'
					? 'Render queued ahead of everything else.'
					: 'Probe due now: the next verification run takes it.'
				: `Nothing queued: ${r.reason}.`;
		} catch (e) {
			notice = String(e);
		}
	}

	function addTag(e: SubmitEvent) {
		e.preventDefault();
		const t = tagDraft.trim().toLowerCase();
		if (!data || !t) return;
		tagDraft = '';
		if (!data.triage.tags.includes(t)) triage({ tags: [...data.triage.tags, t] });
	}

	const cur = $derived(data?.current ?? {});
	const key = $derived(data ? displayKey(data.asset.kind, data.asset.key) : '');
	const url = $derived.by(() => {
		if (!data) return '';
		if (data.asset.kind === 'url') return data.asset.key;
		if (data.asset.kind === 'service' && cur.scheme) {
			const def = (cur.scheme === 'https' && cur.port === 443) || (cur.scheme === 'http' && cur.port === 80);
			return `${cur.scheme}://${cur.host}${def ? '' : ':' + cur.port}/`;
		}
		return '';
	});
	const shot = $derived(shots[shown]);

	const states: [string, string][] = [
		['new', 'New'],
		['seen', 'Seen'],
		['interesting', 'Interesting'],
		['ignored', 'Ignored']
	];

	function show(v: unknown): string {
		if (v === null || v === undefined) return '—';
		if (typeof v === 'string') return v;
		if (Array.isArray(v)) return v.length ? v.join(', ') : '—';
		return JSON.stringify(v);
	}

	function changeText(c: Change): string {
		const what = label(c.kind) + (c.layer ? ` · ${c.layer}` : '');
		return what;
	}

	function diff(c: Change): string {
		const fmt = (v: unknown) => (v && typeof v === 'object' ? JSON.stringify(v) : show(v));
		if (c.before == null) return fmt(c.after);
		return `${fmt(c.before)} → ${fmt(c.after)}`;
	}

	function lineageText(step: Record<string, unknown>): string {
		const { step: s, run, ...rest } = step;
		const extra = Object.entries(rest)
			.map(([k, v]) => `${k} ${show(v)}`)
			.join(', ');
		return label(String(s ?? 'step')) + (extra ? ` (${extra})` : '');
	}
</script>

<svelte:head><title>{key || 'Asset'} · Recon</title></svelte:head>

<div class="page">
	{#if error}
		<p class="error" role="alert">{error}</p>
	{:else if !data}
		<p class="muted">Loading…</p>
	{:else}
		<a class="back" href="/" onclick={(e) => { if (history.length > 1) { e.preventDefault(); history.back(); } }}>← Back to search</a>

		<div class="page-head">
			<div class="title">
				<h1 class="mono">{key}</h1>
				<p>
					{label(data.asset.kind)} in
					{#if program}<a href="/programs/{program.id}">{program.name}</a>{:else}…{/if}
					· {label(data.asset.scope)}
					· <Lifecycle state={cur.lifecycle} />
					· first seen {ago(data.asset.first_seen)}, last seen {ago(data.asset.last_seen)}
				</p>
				{#if url}<p><a class="mono" href={url} target="_blank" rel="noreferrer noopener">{url}</a></p>{/if}
			</div>
			<div class="actions">
				<button class="btn" type="button" onclick={() => action('probe')}>Probe now</button>
				<button class="btn btn-primary" type="button" onclick={() => action('render')}>Render now</button>
			</div>
		</div>
		{#if notice}<p class="notice" role="status">{notice}</p>{/if}

		<section class="panel triage" aria-label="Triage">
			<div role="group" aria-label="Triage state" class="seg-group">
				{#each states as [v, l] (v)}
					<button class="seg" type="button" aria-pressed={data.triage.state === v} onclick={() => triage({ state: v })}>{l}</button>
				{/each}
			</div>
			<ul class="tags" aria-label="Tags">
				{#each data.triage.tags as t (t)}
					<li class="chip">
						<a href={'/' + qs({ q: 'tag:' + t })}>{t}</a>
						<button type="button" aria-label="Remove tag {t}" onclick={() => triage({ tags: data!.triage.tags.filter((x) => x !== t) })}>×</button>
					</li>
				{/each}
				<li>
					<form onsubmit={addTag}>
						<label for="tag" class="sr-only">Add a tag</label>
						<input id="tag" class="input tag-input" placeholder="Add a tag" bind:value={tagDraft} />
					</form>
				</li>
			</ul>
			<div class="note">
				<label for="note" class="sr-only">Note</label>
				<textarea
					id="note"
					class="textarea"
					rows="2"
					placeholder="A note for later"
					bind:value={note}
					oninput={() => (noteSaved = false)}
					onblur={() => {
						if (!noteSaved) triage({ note }).then((ok) => (noteSaved = ok));
					}}
				></textarea>
				<span class="muted saved" aria-live="polite">{noteSaved ? '' : 'Not saved yet: saved when you leave the field'}</span>
			</div>
		</section>

		<div class="cols">
			<div class="main">
				<section class="panel" aria-label="Screenshot">
					<h2>Screenshot</h2>
					{#if shot}
						<a href={shot.url} target="_blank" rel="noreferrer">
							<img class="shot big" src={shot.url} alt="Screenshot of {key}, {dateTime(shot.taken_at)}" width={shot.width} height={shot.height} />
						</a>
						<p class="muted small">Taken {ago(shot.taken_at)} · {shot.width}×{shot.height}</p>
						{#if shots.length > 1}
							<div class="earlier" role="group" aria-label="Distinct screenshots">
								{#each shots as s, i (s.id)}
									<button type="button" class="thumb" aria-pressed={i === shown} aria-label="Screenshot from {dateTime(s.taken_at)}" onclick={() => (shown = i)}>
										<img class="shot" src={s.thumbnail_url} alt="" width="120" height="75" />
									</button>
								{/each}
							</div>
						{/if}
					{:else}
						<p class="muted">No screenshot yet.{#if data.asset.kind === 'service' || data.asset.kind === 'url'} Render now takes one.{/if}</p>
					{/if}
				</section>

				<section class="panel" aria-label="Technologies">
					<h2>Technologies</h2>
					{#if data.technologies.length}
						<ul class="tech-list">
							{#each data.technologies as t (t.name)}
								<li>
									<details>
										<summary>
											<a href={'/' + qs({ q: 'tech:' + (t.name.includes(' ') ? `"${t.name}"` : t.name) })}>{t.name}</a>
											{#if t.version}<span class="mono ver">{t.version}</span>{/if}
											{#if t.category}<span class="muted cat">{t.category}</span>{/if}
										</summary>
										{#if t.cpe}<p class="mono small">{t.cpe}</p>{/if}
										{#if t.proof}<pre class="mono">{JSON.stringify(t.proof, null, 2)}</pre>{/if}
									</details>
								</li>
							{/each}
						</ul>
					{:else}
						<p class="muted">None detected. Technologies come from the Fingerprinter only.</p>
					{/if}
				</section>

				{#if data.children.length}
					<section class="panel" aria-label="Under this asset">
						<h2>Under this asset</h2>
						<ul class="children">
							{#each data.children as c (c.asset_id)}
								<li>
									<a class="mono" href="/assets/{c.asset_id}">{c.key}</a>
									<Status code={c.status_code} />
									<span class="muted">{c.title ?? ''}</span>
								</li>
							{/each}
						</ul>
					</section>
				{/if}

				<section class="panel" aria-label="Timeline">
					<h2>Timeline</h2>
					{#if changes.length}
						<ol class="timeline">
							{#each changes as c, i (i)}
								<li>
									<time class="num muted" datetime={c.at} title={dateTime(c.at)}>{ago(c.at)}</time>
									<div>
										<strong>{changeText(c)}</strong>
										<p class="mono small diff">{diff(c)}</p>
									</div>
								</li>
							{/each}
						</ol>
					{:else}
						<p class="muted">No change recorded.</p>
					{/if}
				</section>
			</div>

			<aside class="side">
				<section class="panel" aria-label="Current state">
					<h2>Current state</h2>
					<dl class="kv">
						{#if cur.status_code != null}<dt>Status</dt><dd><Status code={cur.status_code} /></dd>{/if}
						{#if cur.title}<dt>Title</dt><dd>{cur.title}</dd>{/if}
						{#if cur.final_url}<dt>Redirects to</dt><dd class="mono">{cur.final_url}</dd>{/if}
						{#if cur.redirect_hosts?.length > 1}<dt>Through</dt><dd class="mono">{show(cur.redirect_hosts)}</dd>{/if}
						{#if cur.server}<dt>Server</dt><dd class="mono">{cur.server}</dd>{/if}
						<dt>Addresses</dt><dd class="mono">{show(cur.ips)}</dd>
						{#if cur.cname?.length}<dt>CNAME</dt><dd class="mono">{show(cur.cname)}</dd>{/if}
						{#if cur.cdn || cur.waf}<dt>CDN, WAF</dt><dd>{[cur.cdn, cur.waf].filter(Boolean).join(' · ')}</dd>{/if}
						{#if cur.asn}<dt>ASN</dt><dd>AS{cur.asn} {cur.asn_org ?? ''}</dd>{/if}
						{#if cur.country}<dt>Country</dt><dd>{cur.country}</dd>{/if}
						{#if cur.header_names?.length}<dt>Headers</dt><dd class="mono">{show(cur.header_names)}</dd>{/if}
						{#if cur.cookie_names?.length}<dt>Cookies</dt><dd class="mono">{show(cur.cookie_names)}</dd>{/if}
						{#if cur.external_hosts?.length}<dt>Talks to</dt><dd class="mono">{show(cur.external_hosts)}</dd>{/if}
						{#if cur.web_sockets?.length}<dt>WebSockets</dt><dd class="mono">{show(cur.web_sockets)}</dd>{/if}
						{#if cur.has_robots || cur.has_sitemap || cur.has_llms_txt}
							<dt>Files</dt><dd class="mono">{[cur.has_robots && 'robots.txt', cur.has_sitemap && 'sitemap.xml', cur.has_llms_txt && 'llms.txt'].filter(Boolean).join(', ')}</dd>
						{/if}
						<dt>Last render</dt><dd>{ago(cur.last_render_at)}</dd>
						<dt>Next render</dt><dd>{cur.next_render_at ? ago(cur.next_render_at) : '—'}</dd>
						{#if cur.kind === 'fqdn'}<dt>Next probe</dt><dd>{due(cur.next_full_at)}</dd>{/if}
					</dl>
					{#if data.layers.some((l) => l.informative_failures + l.non_informative_failures > 0)}
						<h3 class="label">Failures</h3>
						<dl class="kv">
							{#each data.layers as l (l.layer)}
								<dt>{l.layer}</dt>
								<dd>{l.informative_failures} counted, {l.non_informative_failures} not, last ok {ago(l.last_ok_at)}</dd>
							{/each}
						</dl>
					{/if}
				</section>

				{#if data.pivots.length}
					<section class="panel" aria-label="Pivots">
						<h2>Pivots</h2>
						<ul class="pivots">
							{#each data.pivots as p (p.kind + p.value)}
								<li class:generic={p.generic}>
									<a href={'/' + qs({ q: p.query })}>
										<span class="muted kind">{p.kind}</span>
										<span class="mono val">{p.value}</span>
									</a>
									<span class="num muted">
										{#if p.others === 0}only here{:else}{p.others} other{p.others === 1 ? '' : 's'}, {p.programs} program{p.programs === 1 ? '' : 's'}{/if}
									</span>
								</li>
							{/each}
						</ul>
					</section>
				{/if}

				<section class="panel" aria-label="Why it is here">
					<h2>Why it is here</h2>
					<p class="small">Found by <strong>{data.asset.discovery_source}</strong>{#if data.asset.parent_id}, under <a href="/assets/{data.asset.parent_id}">its parent</a>{/if}.</p>
					{#if data.asset.lineage?.length}
						<ol class="lineage">
							{#each data.asset.lineage as step, i (i)}
								<li>
									{lineageText(step)}
									{#if step.run}<a class="small" href="/runs/{step.run}">run</a>{/if}
								</li>
							{/each}
						</ol>
					{/if}
				</section>
			</aside>
		</div>
	{/if}
</div>

<style>
	.back {
		font-size: 13px;
		text-decoration: none;
		width: max-content;
	}
	.title {
		min-width: 0;
	}
	.title h1 {
		font-size: 22px;
		overflow-wrap: anywhere;
	}
	.title p {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		gap: 0 6px;
	}
	.actions {
		display: flex;
		gap: 8px;
	}
	.triage {
		flex-direction: row;
		flex-wrap: wrap;
		align-items: center;
		gap: 12px 20px;
	}
	.tags {
		list-style: none;
		margin: 0;
		padding: 0;
		display: flex;
		gap: 6px;
		flex-wrap: wrap;
		align-items: center;
	}
	.tags .chip a {
		color: inherit;
		text-decoration: none;
	}
	.tags .chip button {
		border: 0;
		background: transparent;
		color: var(--faint);
		cursor: pointer;
		font-size: 14px;
		padding: 0 2px;
	}
	.tag-input {
		height: 26px;
		width: 120px;
		font-size: 12px;
	}
	.note {
		flex: 1 1 280px;
		display: flex;
		flex-direction: column;
		gap: 2px;
	}
	.saved {
		font-size: 11px;
		min-height: 16px;
	}
	.big {
		width: 100%;
		height: auto;
		max-height: 640px;
	}
	.small {
		font-size: 12px;
		margin: 0;
	}
	.earlier {
		display: flex;
		gap: 8px;
		flex-wrap: wrap;
	}
	.thumb {
		padding: 0;
		border: 0;
		background: transparent;
		border-radius: 8px;
		cursor: pointer;
		opacity: 0.6;
	}
	.thumb[aria-pressed='true'] {
		opacity: 1;
		outline: 2px solid var(--accent);
		outline-offset: 2px;
	}
	.tech-list,
	.children,
	.pivots,
	.timeline,
	.lineage {
		list-style: none;
		margin: 0;
		padding: 0;
		display: flex;
		flex-direction: column;
		gap: 8px;
	}
	summary {
		cursor: pointer;
		display: flex;
		gap: 8px;
		align-items: baseline;
	}
	.ver {
		font-size: 12px;
		color: var(--text-body);
	}
	.cat {
		font-size: 12px;
	}
	pre {
		margin: 8px 0 0;
		padding: 10px;
		border-radius: 8px;
		background: var(--bg);
		font-size: 11px;
		overflow-x: auto;
		max-height: 240px;
	}
	.children li {
		display: flex;
		gap: 10px;
		align-items: center;
		flex-wrap: wrap;
		font-size: 13px;
	}
	.timeline li {
		display: grid;
		grid-template-columns: 110px 1fr;
		gap: 12px;
		font-size: 13px;
	}
	.timeline time {
		font-size: 12px;
	}
	.diff {
		color: var(--muted);
		overflow-wrap: anywhere;
	}
	.pivots li {
		display: flex;
		flex-direction: column;
		gap: 2px;
		font-size: 13px;
		min-width: 0;
	}
	.pivots a {
		display: flex;
		gap: 8px;
		text-decoration: none;
		min-width: 0;
	}
	.pivots .kind {
		flex: none;
		width: 52px;
	}
	.pivots .val {
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.pivots .num {
		font-size: 12px;
		padding-left: 60px;
	}
	.pivots .generic {
		opacity: 0.62;
	}
	.lineage {
		font-size: 13px;
		gap: 4px;
		padding-left: 18px;
		list-style: decimal;
	}
</style>
