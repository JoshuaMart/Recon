<script lang="ts">
	import Panel from './Panel.svelte';
	import { bytes, codeFamily, landingOf, identity, pivotOf } from '$lib/format';
	import { cookieNamesOf, failureOf, hopsOf, responseHeaders, securityHeaders, type Hop } from '$lib/observation';
	import { badgeFilter, href } from '$lib/query';
	import type { Asset, Evidence } from '$lib/types';

	interface Props {
		asset: Asset;
		evidence?: Evidence;
	}

	const { asset, evidence }: Props = $props();

	/**
	 * The chain, drawn as a descent.
	 *
	 * the projection: a card showing `200` on a service that answered 308, then 307, then 200 was
	 * true and was not the information. The card gained the codes; this view has the room
	 * to show what each hop said and where it sent, which is the part that separates "an
	 * application behind a redirect" from "a service that sends somewhere else entirely".
	 *
	 * The payload is preferred and the promoted column is the fallback: `status_chain` is
	 * always there and carries codes alone, the payload carries the urls too.
	 */
	const hops = $derived.by<Hop[]>(() => {
		const measured = hopsOf(evidence);
		if (measured.length > 0) return measured;
		return (asset.status_chain ?? []).map((code, index, all) => ({
			code,
			url: index === 0 ? identity(asset).head : index === all.length - 1 ? asset.final_url : undefined
		}));
	});

	const headers = $derived(responseHeaders(evidence));
	const headerCount = $derived(Object.keys(headers).length);
	const security = $derived(securityHeaders(headers));
	const present = $derived(security.filter((header) => header.value !== undefined).length);
	const cookies = $derived(cookieNamesOf(evidence));
	const failure = $derived(failureOf(evidence));
	const landing = $derived(landingOf(asset, identity(asset).head));
	const size = $derived(bytes(hops.at(-1)?.size));
	const contentType = $derived(hops.at(-1)?.contentType ?? headers['content-type']);

	/** A cookie name is a pivot, so its counter comes from the projection like any other. */
	function cookieCount(name: string): number | undefined {
		return pivotOf(asset, 'cookie_name', name)?.count;
	}

	function cookieHref(name: string): string {
		return href([badgeFilter('cookie_name', name)]);
	}
</script>

<Panel title="HTTP response" {evidence} meta="never observed">
	{#if hops.length === 0}
		<p class="dv-said">
			<span class="q">Asked</span>
			<span class="mono">{identity(asset).head}</span>
		</p>
		<p class="dv-said">
			<span class="q">Answered</span>
			<span class="mono">{failure ?? 'nothing usable'}</span>
		</p>
		<div class="dv-divide"></div>
		<!-- Four absences of measurement, and not one of them says the service has none. -->
		<div class="dv-row">
			<span class="dv-badge absent">no status code</span>
			<span class="dv-badge absent">no title</span>
			<span class="dv-badge absent">no header read</span>
			<span class="dv-badge absent">no cookie observed</span>
		</div>
	{:else}
		<div class="hops">
			{#each hops as hop, i (i)}
				{#if i > 0}<div class="rule"></div>{/if}
				<div class="hop">
					<span class="dv-code" data-code={codeFamily(hop.code)}>{hop.code ?? '—'}</span>
					<span class="url">{hop.url ?? 'the next hop'}</span>
					<span class="m">
						{#if hop.location}
							location: {hop.location}
						{:else if i === hops.length - 1}
							{[contentType, size, asset.title].filter(Boolean).join(' · ')}
						{/if}
					</span>
				</div>
			{/each}
		</div>

		{#if landing}
			<p class="dv-note landed">The chain lands on <span class="mono">{landing}</span>.</p>
		{/if}

		{#if cookies.length > 0}
			<div class="dv-divide"></div>
			<div class="dv-row">
				<span class="dv-lbl">Sets</span>
				{#each cookies as name (name)}
					{@const count = cookieCount(name)}
					{#if count}
						<a class="dv-badge plain" href={cookieHref(name)} title="{count - 1} other assets set this cookie">
							<span class="v">{name}</span><span class="n">{count}</span>
						</a>
					{:else}
						<span class="dv-badge plain"><span class="v">{name}</span></span>
					{/if}
				{/each}
			</div>
		{/if}

		<div class="dv-divide"></div>

		<!-- Present or absent, with the value. No grade and no letter: a header is a fact,
		     and a score would be the composite severity milestone 7 forbids on an asset. An
		     absent one is dashed and grey rather than red, for the same reason. -->
		<div class="dv-lbl heading">Security headers · {present} of {security.length} present</div>
		<div class="sec">
			{#each security as header (header.name)}
				<div class="sh" class:off={!header.value}>
					<span class="mark">{header.value ? '✓' : '–'}</span>
					<span class="text">
						<b>{header.label}</b>
						<span title={header.value ?? ''}>{header.value ?? 'not sent'}</span>
					</span>
				</div>
			{/each}
		</div>

		{#if headerCount > 0}
			<p class="dv-note more">
				{headerCount} response {headerCount === 1 ? 'header' : 'headers'} in all.
				<a class="link" href="#evidence">Read the raw payload</a>
			</p>
		{/if}
	{/if}
</Panel>

<style>
	.hops {
		display: grid;
		min-width: 0;
	}

	.hop {
		display: flex;
		align-items: center;
		gap: 9px;
		min-width: 0;
	}

	.hop .url {
		font-family: var(--font-mono);
		font-size: 12.5px;
		color: var(--ink);
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
		flex: none;
		max-width: 55%;
	}

	.hop .m {
		font-size: 11.5px;
		color: var(--ink-3);
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
		min-width: 0;
	}

	.rule {
		width: 1px;
		height: 13px;
		background: var(--border);
		margin-left: 20px;
	}

	.landed {
		margin-top: 8px;
	}

	.heading {
		margin-bottom: 7px;
	}

	.sec {
		display: grid;
		grid-template-columns: repeat(auto-fit, minmax(280px, 1fr));
		gap: 7px;
	}

	.sh {
		display: flex;
		align-items: flex-start;
		gap: 7px;
		border: 1px solid var(--border-2);
		border-radius: var(--radius-control);
		padding: 6px 8px;
		min-width: 0;
	}

	.sh .mark {
		color: var(--signal);
		font-size: 12px;
		line-height: 1.2;
		flex: none;
	}

	.sh .text {
		min-width: 0;
	}

	.sh b {
		display: block;
		font-size: 11px;
		font-weight: 600;
		color: var(--ink-2);
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.sh .text span {
		display: block;
		font-family: var(--font-mono);
		font-size: 10px;
		color: var(--ink-3);
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.sh.off {
		border-style: dashed;
	}

	.sh.off .mark {
		color: var(--ink-3);
	}

	.sh.off b {
		color: var(--ink-3);
		font-weight: 500;
		font-style: italic;
	}

	.more {
		margin-top: 10px;
		font-size: 11.5px;
	}
</style>
