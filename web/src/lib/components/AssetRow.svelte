<script lang="ts">
	import Icon from './Icon.svelte';
	import {
		ago,
		codeFamily,
		cookieState,
		exact,
		identity,
		serviceURL,
		statusLabel,
		verdictOf,
		webSurface,
		landingOf
	} from '$lib/format';
	import { rowBadges, type Shared } from '$lib/group';
	import { badgeFilter, href, withFilter, type Filter } from '$lib/query';
	import type { Asset } from '$lib/types';

	interface Props {
		asset: Asset;
		filters: Filter[];
		/** What the host header already states, so the row does not repeat it. */
		shared?: Shared;
		/** Flat mode writes the host into the row, since no header carries it. */
		withHost?: boolean;
	}

	const { asset, filters, shared = {}, withHost = false }: Props = $props();

	/**
	 * One service, one line (the fold).
	 *
	 * The card it replaces spent 160 px per asset and repeated its host's operator, city
	 * and certificate on every one of them. What the row keeps is everything milestone 7
	 * asserts: the lifecycle as a colour **and** a sentence where the colour alone would
	 * conclude, the status with its chain, the three states of the cookie badge, and no
	 * score anywhere.
	 */
	const name = $derived(identity(asset));
	const label = $derived(statusLabel(asset));
	const hops = $derived((asset.status_chain ?? []).slice(0, -1));
	const verdict = $derived(verdictOf(asset));
	const badges = $derived(rowBadges(asset, shared));
	const cookies = $derived(cookieState(asset));
	const openable = $derived(asset.kind === 'url' ? asset.key : serviceURL(asset));

	/**
	 * Where the chain landed, and nothing when it landed on itself.
	 *
	 * `landingOf` rather than a comparison written here: a service reached on `:443`
	 * answers a final URL that spells the port the canonical form drops, so the two
	 * strings differ while the address does not. The path alone when the host is
	 * unchanged, the whole URL when it is, because a service that sends somewhere else
	 * is a different fact and the host is the half of it that matters.
	 */
	const landing = $derived(landingOf(asset, name.head));

	/** The port and the scheme the probe measured, or the host when nothing groups it. */
	const head = $derived.by(() => {
		if (withHost) return { main: name.head, sub: '' };
		if (asset.kind === 'service') return { main: asset.port ? String(asset.port) : name.head, sub: asset.scheme ?? '' };
		return { main: name.head, sub: '' };
	});

	function pivotHref(type: string, value: string): string {
		return href(withFilter(filters, badgeFilter(type, value)));
	}

	/**
	 * The takeover candidate, which is the finding this whole product exists for.
	 *
	 * First in the cell and loud, because it is the one badge that is not a pivot:
	 * it does not link assets, it says one of them points at something anybody can
	 * now claim. It still links to a search, on the field that asks whether the key
	 * is there at all, so "show me every one of these" is one click.
	 */
	const takeover = $derived(asset.attributes?.takeover_candidate);
	const takeoverHref = $derived(
		href(withFilter(filters, { field: 'takeover_candidate', op: 'exists', value: 'true' }))
	);

	/**
	 * What a badge from the fingerprinter says about its own age.
	 *
	 * The service runs on five triggers rather than on a cadence, so a technology,
	 * a favicon or a cookie can be weeks older than the last probe. A badge sharing
	 * the row's `last_checked_at` would be claiming the browser saw what the probe
	 * saw, and the gap is exactly what somebody needs to know before trusting it.
	 *
	 * The certificate is not one of these: it comes from the probe, like the status
	 * code beside it.
	 */
	function rendered(type: string): string {
		if (type === 'cert_spki') return '';
		if (!asset.last_fingerprint_at) return '';
		return ' Rendered ' + ago(asset.last_fingerprint_at) + ', which is not when the row was last probed.';
	}
</script>

<div class="row" class:flat={withHost} class:muted={asset.lifecycle === 'inactive' || asset.lifecycle === 'archived'}>
	<span class="dot {asset.lifecycle}" title={asset.lifecycle}></span>

	<a class="head" href="/assets/{asset.asset_id}" title={asset.key}>
		<span class="main">{head.main}</span>
		{#if head.sub}<em>{head.sub}</em>{/if}
	</a>

	<span class="status">
		<!-- The hops that led here, then the code that answered. A single-hop chain
		     yields nothing: an arrow pointing at one code is noise on most of a list. -->
		{#each hops as hop, i (i)}
			<span class="hop" data-code={codeFamily(hop)}>{hop}</span>
		{/each}
		<span class="code" class:none={!label.family} data-code={label.family}>{label.code}</span>
	</span>

	<!-- An unobservable or an inactive asset carries its sentence here. It needs an
	     explicit mention rather than a colour alone, and an asset no observer reaches has
	     no title to show in this cell anyway. -->
	{#if verdict}
		<span class="title verdict {verdict.tone}" title={verdict.text}>{verdict.text}</span>
	{:else if asset.title}
		<span class="title" title={asset.title}>{asset.title}</span>
	{:else}
		<span class="title none">no title</span>
	{/if}

	<span class="land" title={asset.final_url ?? ''}>
		{#if landing}→ {landing}{/if}
	</span>

	<span class="pivots">
		{#if takeover}
			<a
				class="pv takeover"
				href={takeoverHref}
				title="A dangling reference to {takeover.target ?? 'an unclaimed target'}{takeover.kind
					? ' (' + takeover.kind + ')'
					: ''}. Somebody else can claim it."
			>
				<span class="k">takeover</span>
				<span class="v">{takeover.kind ?? 'candidate'}</span>
			</a>
		{/if}

		{#each badges as badge (badge.type + badge.value)}
			<a
				class="pv"
				class:many={badge.count > 3}
				href={pivotHref(badge.type, badge.value)}
				title="{badge.count - 1} other assets share this {badge.type === 'cert_spki'
					? 'certificate key'
					: badge.type}.{rendered(badge.type)}"
			>
				<span class="k"
					>{badge.type === 'cert_spki' ? 'cert' : badge.type === 'cookie_name' ? 'cookie' : badge.type}</span
				>
				<span class="v">{badge.type === 'cookie_name' ? badge.value : badge.value.slice(0, 8)}</span>
				<span class="n">{badge.count}</span>
			</a>
		{/each}

		<!-- The three states of the three absences, kept on the row. An absence of data must not
		     read as data, so a missing cookie badge says which absence it is. -->
		{#if !webSurface(asset)}
			<!-- Nothing renders a name, so "never rendered" would read as a pending
			     state rather than as a rule. The row says nothing, which is the honest
			     amount. -->
		{:else if cookies === 'never-rendered'}
			<span
				class="pv absent"
				title="The fingerprinter has never rendered this asset, so the missing cookies, favicon and technologies say nothing about what it serves."
			>
				never rendered
			</span>
		{:else if cookies === 'none' && badges.length === 0}
			<span class="pv absent" title="Rendered {ago(asset.last_fingerprint_at)}, and it sets no cookie.">no cookie</span>
		{:else if cookies === 'all-filtered' && badges.length === 0}
			<span
				class="pv absent"
				title="Rendered {ago(
					asset.last_fingerprint_at
				)}. It sets cookies, and every name is either generic or unique to this asset, so none earns a badge."
			>
				cookies, none worth a badge
			</span>
		{/if}
	</span>

	<!-- No threshold colour on the number: the bands the volatility rule wants need weeks of real
	     data, and painting it would be inventing them. -->
	<span class="vol" title="{asset.volatility} {asset.volatility === 1 ? 'change' : 'changes'} in 7 days">
		<Icon name="trend" />
		<b>{asset.volatility}</b>
	</span>

	<span class="age" title={exact(asset.last_seen)}>{ago(asset.last_seen).replace(' ago', '')}</span>

	<span class="acts">
		{#if openable}
			<!-- `noopener` stops the opened page reaching window.opener and navigating this
			     tab to a fake login, `noreferrer` keeps the console URL and its filters out
			     of the target's Referer. -->
			<a
				class="launch"
				href={openable}
				target="_blank"
				rel="noopener noreferrer"
				referrerpolicy="no-referrer"
				title="Open {openable} in a new tab"
				aria-label="Open in a new tab"
			>
				<Icon name="open" />
			</a>
		{/if}
		<a class="go" href="/assets/{asset.asset_id}" aria-label="Open this asset"><Icon name="chevron" /></a>
	</span>
</div>

<style>
	.row {
		display: grid;
		grid-template-columns: 12px 78px 118px minmax(110px, 1.3fr) minmax(0, 1fr) 216px 44px 40px 40px;
		gap: 10px;
		align-items: center;
		padding: 0 12px;
		height: 31px;
		border-top: 1px solid var(--border-2);
		min-width: 0;
	}

	/* Flat, the host is back in the row and it is what identifies the line. The grouped
	   row can spend its width on the title because the address is three columns wide in
	   the header above it; here the address has to be readable first. */
	.row.flat {
		grid-template-columns: 12px minmax(230px, 1.7fr) 118px minmax(90px, 0.9fr) minmax(0, 0.8fr) 216px 44px 40px 40px;
	}

	.row.flat .title {
		font-size: 12px;
		color: var(--ink-2);
	}

	.row:hover {
		background: #fcfdfd;
	}

	.row.muted {
		background: #fcfcfc;
	}

	.dot {
		width: 7px;
		height: 7px;
		border-radius: 50%;
		background: var(--signal);
	}

	.dot.flapping {
		background: var(--code-4xx);
	}

	.dot.inactive,
	.dot.archived {
		background: var(--dead);
	}

	.dot.unobservable {
		background: var(--unobs);
	}

	.dot.candidate {
		background: transparent;
		border: 2px solid var(--ink-3);
		width: 8px;
		height: 8px;
	}

	.head {
		display: flex;
		align-items: baseline;
		gap: 5px;
		min-width: 0;
		text-decoration: none;
	}

	.head .main {
		font-family: var(--font-mono);
		font-size: 12.5px;
		color: var(--ink);
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.head:hover .main {
		text-decoration: underline;
		text-decoration-color: var(--signal);
	}

	.head em {
		font-style: normal;
		font-size: 9.5px;
		text-transform: uppercase;
		letter-spacing: 0.04em;
		color: var(--ink-3);
		flex: none;
	}

	.status {
		display: flex;
		align-items: center;
		gap: 4px;
		min-width: 0;
	}

	/* A hop is a status code and reads as one: same chip, one size down and without
	   the weight, so the code that answered still dominates the row. Leaving the hops
	   as bare text made a 308 look like a different kind of thing from the 200 it led
	   to, when it is the same measurement one step earlier. */
	.hop {
		font-family: var(--font-mono);
		font-size: 10.5px;
		font-weight: 500;
		border-radius: var(--radius-control);
		padding: 1px 5px;
		color: var(--ink-3);
		background: var(--canvas);
	}

	.hop[data-code='2xx'] {
		color: var(--code-2xx);
		background: var(--code-2xx-bg);
	}

	.hop[data-code='3xx'] {
		color: var(--code-3xx);
		background: var(--code-3xx-bg);
	}

	.hop[data-code='4xx'] {
		color: var(--code-4xx);
		background: var(--code-4xx-bg);
	}

	.hop[data-code='5xx'] {
		color: var(--code-5xx);
		background: var(--code-5xx-bg);
	}

	.code {
		font-family: var(--font-mono);
		font-size: 11.5px;
		font-weight: 600;
		border-radius: var(--radius-control);
		padding: 1px 6px;
		white-space: nowrap;
	}

	.code[data-code='2xx'] {
		color: var(--code-2xx);
		background: var(--code-2xx-bg);
	}

	.code[data-code='3xx'] {
		color: var(--code-3xx);
		background: var(--code-3xx-bg);
	}

	.code[data-code='4xx'] {
		color: var(--code-4xx);
		background: var(--code-4xx-bg);
	}

	.code[data-code='5xx'] {
		color: var(--code-5xx);
		background: var(--code-5xx-bg);
	}

	.code.none {
		color: var(--ink-3);
		background: var(--canvas);
		font-weight: 500;
		font-size: 11px;
	}

	.title {
		font-size: 12.5px;
		color: var(--ink);
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.title.none {
		color: var(--ink-3);
		font-style: italic;
		font-size: 12px;
	}

	.title.verdict {
		font-size: 11.5px;
		color: var(--ink-2);
		border-left: 2px solid var(--unobs);
		padding-left: 7px;
	}

	.title.verdict.dead {
		border-left-color: var(--dead);
		color: var(--ink-3);
	}

	.land {
		font-family: var(--font-mono);
		font-size: 11px;
		color: var(--ink-3);
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.pivots {
		display: flex;
		align-items: center;
		justify-content: flex-end;
		gap: 4px;
		min-width: 0;
	}

	.pv {
		display: inline-flex;
		align-items: center;
		gap: 4px;
		font-size: 10.5px;
		border-radius: var(--radius-control);
		padding: 1px 5px;
		background: var(--canvas);
		color: var(--ink-2);
		text-decoration: none;
		min-width: 0;
		white-space: nowrap;
		overflow: hidden;
	}

	.pv:hover {
		color: var(--ink);
	}

	.pv .k {
		font-size: 9.5px;
		text-transform: uppercase;
		letter-spacing: 0.04em;
		opacity: 0.6;
		flex: none;
	}

	.pv .v {
		font-family: var(--font-mono);
		overflow: hidden;
		text-overflow: ellipsis;
	}

	.pv .n {
		font-family: var(--font-mono);
		font-weight: 600;
		font-size: 10px;
		flex: none;
	}

	/* The counter is what turns an attribute into a lead, so a widely shared pivot is
	   loud and one shared with a single other asset is not. */
	.pv.many {
		background: var(--signal-bg);
		color: #0a7a58;
	}

	/* The one badge that is not a pivot, and the only red on a row. Everything
	   else here is evidence; this is a finding. */
	.pv.takeover {
		background: var(--code-5xx-bg);
		color: var(--code-5xx);
		font-weight: 500;
	}

	.pv.takeover .k {
		opacity: 0.8;
	}

	.pv.absent {
		background: transparent;
		border: 1px dashed var(--border);
		color: var(--ink-3);
		font-style: italic;
	}

	.vol {
		display: flex;
		align-items: center;
		justify-content: flex-end;
		gap: 3px;
		font-family: var(--font-mono);
		font-size: 11px;
		color: var(--ink-3);
	}

	.vol :global(svg) {
		width: 11px;
		height: 11px;
	}

	.vol b {
		font-weight: 600;
		color: var(--ink-2);
	}

	.age {
		font-size: 11px;
		color: var(--ink-3);
		text-align: right;
	}

	.acts {
		display: flex;
		align-items: center;
		justify-content: flex-end;
		gap: 2px;
		color: var(--border);
	}

	.acts :global(svg) {
		width: 12px;
		height: 12px;
	}

	.launch:hover,
	.go:hover {
		color: var(--ink-2);
	}
</style>
