<script lang="ts">
	import Panel from './Panel.svelte';
	import { ago, pivotOf, versionOf } from '$lib/format';
	import { renderFacts, scriptsOf } from '$lib/observation';
	import { badgeFilter, href, withFilter } from '$lib/query';
	import type { Asset, Evidence } from '$lib/types';

	interface Props {
		asset: Asset;
		evidence?: Evidence;
		/** The favicon this screen read out of the journal, when the render carried one. */
		favicon?: string;
	}

	const { asset, evidence, favicon }: Props = $props();

	const facts = $derived(renderFacts(evidence));
	const scripts = $derived(scriptsOf(evidence));
	const technologies = $derived(asset.technologies ?? []);
	const faviconBadge = $derived(pivotOf(asset, 'favicon'));

	/**
	 * How many scripts are shown before the rest are folded.
	 *
	 * the three absences took script hashes off the card on a measurement — 464 badges for 50 rows,
	 * 316 distinct values — and said in the same breath that they stay a pivot in the
	 * search and in this view. A list with a counter is the granularity that was missing:
	 * this screen shows one asset, so a dozen rows are readable where a dozen badges on
	 * fifty rows were not.
	 */
	const shown = 6;
	let all = $state(false);
	const visible = $derived(all ? scripts : scripts.slice(0, shown));

	function scriptCount(hash: string | undefined): number | undefined {
		if (!hash) return undefined;
		return pivotOf(asset, 'script', hash)?.count;
	}

	/** The path alone: the host is the asset, and repeating it once per script is noise. */
	function pathOf(url: string | undefined): string {
		if (!url) return 'inline script';
		try {
			const parsed = new URL(url);
			return parsed.pathname + parsed.search;
		} catch {
			return url;
		}
	}
</script>

<Panel title="Rendered page" {evidence} meta="never rendered">
	{#if !evidence}
		<p class="dv-said">
			The fingerprinter has never rendered this asset. It is triggered by a usable http response and there has not been
			one, so the missing favicon, technologies and cookie names are the consequence of that rather than a description
			of the service.
		</p>
		<div class="dv-row gap">
			<span class="dv-badge absent">never rendered</span>
			<span class="says">
				a protected target often sits here the longest, which is what makes it worth a look by hand
			</span>
		</div>
	{:else}
		<div class="dv-row top">
			{#if favicon}
				<!-- In an <img>, which executes nothing even handed an svg. -->
				<img class="fav" src={favicon} alt="" />
			{/if}
			<div class="pivots">
				<div class="dv-row">
					{#if faviconBadge}
						<a
							class="dv-badge hash"
							href={href([badgeFilter('favicon', faviconBadge.value)])}
							title="{faviconBadge.count - 1} other assets share this favicon"
						>
							<span class="k">favicon</span><span class="v">{faviconBadge.value}</span><span class="n"
								>{faviconBadge.count}</span
							>
						</a>
					{:else if facts.faviconHash}
						<span class="dv-badge plain"><span class="k">favicon</span><span class="v">{facts.faviconHash}</span></span>
					{:else}
						<span class="dv-badge absent">no favicon</span>
					{/if}

					{#each technologies as technology (technology)}
						<a
							class="dv-badge plain"
							href={href(withFilter([], { field: 'technologies', op: 'contains', value: technology }))}
						>
							<span class="v">{technology}</span>
							{#if versionOf(asset, technology)}<span class="ver">{versionOf(asset, technology)}</span>{/if}
						</a>
					{/each}
					{#if technologies.length === 0}
						<span class="dv-badge absent">no technology recognised</span>
					{/if}

					{#each facts.cookieNames as name (name)}
						<a class="dv-badge plain" href={href([badgeFilter('cookie_name', name)])}><span class="v">{name}</span></a>
					{/each}
				</div>
				{#if asset.last_fingerprint_at && asset.last_checked_at}
					<p class="dv-note gap">
						Rendered {ago(asset.last_fingerprint_at)}, against a probe {ago(asset.last_checked_at)}. The fingerprinter
						runs on triggers rather than on a cadence, so the two clocks are meant to differ.
					</p>
				{/if}
			</div>
		</div>

		{#if scripts.length > 0}
			<div class="dv-divide"></div>
			<div class="dv-row between">
				<span class="dv-lbl">Internal scripts · {scripts.length}</span>
				<span class="says">each one is a pivot, with its own counter</span>
			</div>
			<div class="files">
				{#each visible as script, i (script.hash ?? i)}
					<div class="file">
						<span class="p" title={script.url ?? ''}>{pathOf(script.url)}</span>
						{#if script.hash}
							<a class="h" href={href([badgeFilter('script', script.hash)])}>{script.hash.slice(0, 8)}…</a>
							<span class="c">{scriptCount(script.hash) ?? ''}</span>
						{:else}
							<span class="h">—</span>
							<span class="c"></span>
						{/if}
					</div>
				{/each}
			</div>
			{#if scripts.length > shown}
				<p class="dv-note gap">
					<button class="link" type="button" onclick={() => (all = !all)}>
						{all ? 'Show the first ' + shown : 'Show the ' + (scripts.length - shown) + ' others'}
					</button>
				</p>
			{/if}
		{/if}

		<div class="dv-divide"></div>
		<div class="dv-row">
			<span class="dv-lbl">Page</span>
			{#if facts.robots !== undefined}
				<span class="dv-badge" class:plain={facts.robots} class:absent={!facts.robots}>
					<span class="v">{facts.robots ? 'robots.txt' : 'no robots.txt'}</span>
				</span>
			{/if}
			{#if facts.llms !== undefined}
				<span class="dv-badge" class:plain={facts.llms} class:absent={!facts.llms}>
					<span class="v">{facts.llms ? 'llms.txt' : 'no llms.txt'}</span>
				</span>
			{/if}
			{#if facts.cname}
				<span class="dv-badge plain"><span class="k">cname</span><span class="v">{facts.cname}</span></span>
			{/if}
			{#if facts.externalHosts.length > 0}
				{#each facts.externalHosts as host (host)}
					<a class="dv-badge plain" href={href([{ field: 'external_hosts', op: 'contains', value: host }])}>
						<span class="k">external</span><span class="v">{host}</span>
					</a>
				{/each}
			{:else}
				<span class="dv-badge absent">no external host</span>
			{/if}
		</div>
	{/if}
</Panel>

<style>
	.top {
		gap: 12px;
		flex-wrap: nowrap;
		align-items: flex-start;
	}

	.fav {
		width: 34px;
		height: 34px;
		border-radius: 5px;
		image-rendering: pixelated;
		flex: none;
		border: 1px solid var(--border-2);
	}

	.pivots {
		flex: 1;
		min-width: 0;
	}

	.gap {
		margin-top: 7px;
	}

	.between {
		justify-content: space-between;
	}

	.says {
		font-size: 11.5px;
		color: var(--ink-3);
	}

	.dv-badge .ver {
		font-family: var(--font-mono);
		font-size: 10.5px;
		color: var(--ink-3);
	}

	.files {
		display: grid;
		margin-top: 8px;
	}

	.file {
		display: grid;
		grid-template-columns: minmax(0, 1fr) 96px 34px;
		gap: 10px;
		align-items: center;
		padding: 4px 0;
		font-size: 11.5px;
		border-bottom: 1px solid var(--border-2);
	}

	.file .p {
		font-family: var(--font-mono);
		color: var(--ink-2);
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.file .h {
		font-family: var(--font-mono);
		font-size: 11px;
		color: var(--ink-3);
		text-decoration: none;
	}

	.file a.h:hover {
		color: var(--ink);
	}

	.file .c {
		text-align: right;
		font-family: var(--font-mono);
		font-size: 10.5px;
		color: var(--ink-3);
	}
</style>
