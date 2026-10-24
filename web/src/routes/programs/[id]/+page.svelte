<script lang="ts">
	import { page } from '$app/state';
	import { api, ApiError, qs } from '#lib/api.ts';
	import { ago, count, dateTime, label } from '#lib/format.ts';
	import type { Program, Rule, Wildcard } from '#lib/types.ts';

	interface Untracked {
		value: string;
		type: string;
		reason: string;
		since: string;
	}
	interface Detail extends Omit<Program, 'wildcards'> {
		rules: Rule[];
		wildcards: Wildcard[];
		untracked: Untracked[];
	}
	interface Declared {
		entry: string;
		kind?: string;
		key?: string;
		created: boolean;
		scope?: string;
		error?: string;
	}

	const id = $derived(page.params.id ?? '');

	let p = $state<Detail | null>(null);
	let error = $state('');
	let notice = $state('');
	let showClosed = $state(false);
	let rule = $state({ kind: 'include', matcher: 'apex', pattern: '', note: '' });
	let ruleError = $state('');
	let entries = $state('');
	let declared = $state<Declared[]>([]);
	let declaring = $state(false);

	$effect(() => {
		load(id);
	});

	async function load(programID: string) {
		try {
			p = await api.get<Detail>(`/api/programs/${programID}`);
		} catch (e) {
			error = e instanceof ApiError && e.status === 404 ? 'No program has this id.' : String(e);
		}
	}

	async function patch(body: Record<string, unknown>) {
		notice = '';
		try {
			await api.patch(`/api/programs/${id}`, body);
			await load(id);
		} catch (e) {
			notice = e instanceof ApiError ? e.message : String(e);
		}
	}

	async function addRule(e: SubmitEvent) {
		e.preventDefault();
		ruleError = '';
		try {
			await api.post(`/api/programs/${id}/rules`, { ...rule, note: rule.note || undefined });
			rule.pattern = '';
			rule.note = '';
			await load(id);
		} catch (err) {
			ruleError = err instanceof ApiError ? err.message : String(err);
		}
	}

	async function closeRule(r: Rule) {
		notice = '';
		try {
			await api.patch(`/api/programs/${id}/rules/${r.id}`, { close: true });
			await load(id);
		} catch (e) {
			notice = e instanceof ApiError ? e.message : String(e);
		}
	}

	async function declare(e: SubmitEvent) {
		e.preventDefault();
		declaring = true;
		try {
			const list = entries.split(/[\s,]+/).filter(Boolean);
			declared = (await api.post<{ entries: Declared[] }>(`/api/programs/${id}/assets`, { entries: list })).entries;
			entries = '';
		} catch (err) {
			notice = err instanceof ApiError ? err.message : String(err);
		} finally {
			declaring = false;
		}
	}

	async function scan(w: Wildcard) {
		notice = '';
		try {
			const r = await api.post<{ queued: boolean; reason?: string }>(`/api/wildcards/${w.id}/recon`);
			notice = r.queued ? `Recon queued on ${w.apex}.` : `${w.apex}: ${r.reason}.`;
		} catch (err) {
			notice = `${w.apex}: ${err instanceof ApiError ? err.message : String(err)}`;
		}
	}

	const rules = $derived((p?.rules ?? []).filter((r) => showClosed || r.in_force));
	const closed = $derived((p?.rules ?? []).filter((r) => !r.in_force).length);
	const searchHref = $derived(p ? '/' + qs({ q: `program:"${p.name}"` }) : '/');
	const autoValue = $derived(p?.auto_scan ?? 'inherit');
</script>

<svelte:head><title>{p?.name ?? 'Program'} · Recon</title></svelte:head>

<div class="page">
	{#if error}
		<p class="error" role="alert">{error}</p>
	{:else if !p}
		<p class="muted">Loading…</p>
	{:else}
		<a class="back" href="/programs">← Programs</a>
		<div class="page-head">
			<div>
				<h1>{p.name}</h1>
				<p>
					{p.origin === 'manual' ? 'Manual program' : `From ScopesExtractor · ${p.platform ?? ''}${p.slug ? ' · ' + p.slug : ''}`}
					{#if p.bounty === false} · VDP{/if}
					· created {ago(p.created_at)}
					{#if p.archived_by} · archived by {p.archived_by}{/if}
				</p>
			</div>
			<div class="actions">
				<a class="btn" href={searchHref}>Search its assets</a>
			</div>
		</div>
		{#if notice}<p class="notice" role="status">{notice}</p>{/if}

		<section class="panel settings" aria-label="Settings">
			<div class="setting">
				<span class="label">State</span>
				<div role="group" aria-label="State" class="seg-group">
					{#each ['active', 'paused', 'archived'] as s (s)}
						<button class="seg" type="button" aria-pressed={p.state === s} onclick={() => patch({ state: s })}>{s.replace(/^./, (c) => c.toUpperCase())}</button>
					{/each}
				</div>
			</div>
			<div class="setting">
				<label class="label" for="auto">Auto-scan of new wildcards</label>
				<select
					id="auto"
					class="select"
					value={autoValue}
					onchange={(e) => patch({ auto_scan: e.currentTarget.value === 'inherit' ? null : e.currentTarget.value })}
				>
					<option value="inherit">Global default{p.auto_scan ? '' : ` (${label(p.effective_auto_scan)})`}</option>
					<option value="off">Off</option>
					<option value="on_new">On new</option>
				</select>
			</div>
			<p class="muted small">Paused and archived programs get no probes and no renders. Nothing is deleted.</p>
		</section>

		<div class="cols">
			<div class="main">
				<section class="panel" aria-label="Scope rules">
					<div class="head">
						<h2>Scope rules</h2>
						{#if closed}
							<label class="toggle"><input type="checkbox" bind:checked={showClosed} /> Show {closed} closed</label>
						{/if}
					</div>
					<div class="table-wrap flat">
						<table>
							<thead>
								<tr><th scope="col">Rule</th><th scope="col">Pattern</th><th scope="col">Origin</th><th scope="col">Since</th><th scope="col"><span class="sr-only">Action</span></th></tr>
							</thead>
							<tbody>
								{#each rules as r (r.id)}
									<tr class:closed={!r.in_force}>
										<td><span class="badge {r.kind === 'include' ? 'ok' : 'bad'}">{r.kind}</span> <span class="muted">{label(r.matcher)}</span></td>
										<td class="mono">{r.pattern}{#if r.note}<div class="muted small">{r.note}</div>{/if}</td>
										<td class="muted">{r.origin}</td>
										<td class="muted num" title={dateTime(r.valid_from)}>{r.in_force ? ago(r.valid_from) : 'closed ' + ago(r.valid_to)}</td>
										<td class="end">{#if r.in_force && r.origin === 'local'}<button class="btn btn-sm" type="button" onclick={() => closeRule(r)}>Close</button>{/if}</td>
									</tr>
								{:else}
									<tr><td colspan="5" class="empty">No rule: nothing is in scope yet.</td></tr>
								{/each}
							</tbody>
						</table>
					</div>
					<form class="rule-form" onsubmit={addRule}>
						<label class="sr-only" for="rk">Kind</label>
						<select id="rk" class="select" bind:value={rule.kind}><option value="include">Include</option><option value="exclude">Exclude</option></select>
						<label class="sr-only" for="rm">Matcher</label>
						<select id="rm" class="select" bind:value={rule.matcher}>
							<option value="apex">apex</option>
							<option value="fqdn">fqdn</option>
							<option value="url_prefix">URL prefix</option>
							<option value="cidr">CIDR</option>
						</select>
						<label class="sr-only" for="rp">Pattern</label>
						<input id="rp" class="input mono grow" placeholder="example.com" bind:value={rule.pattern} required />
						<label class="sr-only" for="rn">Note</label>
						<input id="rn" class="input" placeholder="Note (optional)" bind:value={rule.note} />
						<button class="btn btn-primary" type="submit" disabled={!rule.pattern.trim()}>Add rule</button>
					</form>
					{#if ruleError}<p class="error">{ruleError}</p>{/if}
					{#if p.origin !== 'manual'}<p class="muted small">Synced rules follow ScopesExtractor. A local exclusion narrows them.</p>{/if}
				</section>

				<section class="panel" aria-label="Add hosts and URLs">
					<h2>Add hosts and URLs</h2>
					<form class="declare" onsubmit={declare}>
						<label class="sr-only" for="entries">Hosts and URLs, one per line</label>
						<textarea id="entries" class="textarea mono" rows="4" placeholder={'app.example.com\nhttps://example.com/admin'} bind:value={entries}></textarea>
						<button class="btn" type="submit" disabled={declaring || !entries.trim()}>Add and probe</button>
					</form>
					{#if declared.length}
						<ul class="declared">
							{#each declared as d (d.entry)}
								<li>
									<span class="mono">{d.entry}</span>
									{#if d.error}<span class="badge bad">{d.error}</span>
									{:else}<span class="badge {d.scope === 'in_scope' ? 'ok' : 'warn'}">{label(d.scope ?? '')}</span>
										<span class="muted">{d.created ? 'new' : 'already known'}</span>{/if}
								</li>
							{/each}
						</ul>
					{/if}
				</section>

				{#if p.untracked.length}
					<section class="panel" aria-label="Untracked scopes">
						<h2>Untracked scopes</h2>
						<p class="muted small">What the platform lists that Recon does not turn into a rule.</p>
						<ul class="untracked">
							{#each p.untracked as u (u.value)}
								<li><span class="mono">{u.value}</span> <span class="muted">{u.type} · {label(u.reason)}</span></li>
							{/each}
						</ul>
					</section>
				{/if}
			</div>

			<aside class="side">
				<section class="panel" aria-label="Wildcards">
					<h2>Wildcards</h2>
					{#if p.wildcards.length}
						<ul class="wildcards">
							{#each p.wildcards as w (w.id)}
								<li class:inactive={!w.active}>
									<div>
										<span class="mono">*.{w.apex}</span>
										<span class="muted small num">
											{w.last_recon_at ? 'recon ' + ago(w.last_recon_at) : 'never scanned'}{#if w.assets_found} · {count(w.assets_found)} assets{/if}
										</span>
									</div>
									{#if w.active}<button class="btn btn-sm" type="button" aria-label="Scan {w.apex}" onclick={() => scan(w)}>Scan</button>{/if}
								</li>
							{/each}
						</ul>
					{:else}
						<p class="muted small">None. An apex rule makes one.</p>
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
	.settings {
		flex-direction: row;
		flex-wrap: wrap;
		gap: 16px 32px;
		align-items: flex-end;
	}
	.setting {
		display: flex;
		flex-direction: column;
		gap: 6px;
	}
	.small {
		font-size: 12px;
		margin: 0;
	}
	.head {
		display: flex;
		justify-content: space-between;
		align-items: center;
	}
	.head h2 {
		font-size: 11px;
		font-weight: 600;
		letter-spacing: 0.05em;
		text-transform: uppercase;
		color: var(--faint);
	}
	.toggle {
		font-size: 12px;
		color: var(--muted);
		display: flex;
		gap: 6px;
		align-items: center;
	}
	.flat {
		box-shadow: none;
		background: transparent;
	}
	tr.closed td {
		opacity: 0.55;
	}
	.rule-form,
	.declare {
		display: flex;
		gap: 8px;
		flex-wrap: wrap;
		align-items: flex-start;
	}
	.grow {
		flex: 1 1 200px;
	}
	.declare textarea {
		flex: 1 1 320px;
	}
	.declared,
	.untracked,
	.wildcards {
		list-style: none;
		margin: 0;
		padding: 0;
		display: flex;
		flex-direction: column;
		gap: 8px;
		font-size: 13px;
	}
	.declared li {
		display: flex;
		gap: 8px;
		align-items: center;
		flex-wrap: wrap;
	}
	.wildcards li {
		display: flex;
		justify-content: space-between;
		gap: 8px;
		align-items: center;
	}
	.wildcards li div {
		display: flex;
		flex-direction: column;
		min-width: 0;
	}
	.wildcards .inactive {
		opacity: 0.55;
	}
</style>
