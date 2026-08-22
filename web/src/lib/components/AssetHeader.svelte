<script lang="ts">
	import Icon from './Icon.svelte';
	import { ago, exact, identity, serviceURL, verdictOf } from '$lib/format';
	import { href } from '$lib/query';
	import type { Asset } from '$lib/types';

	interface Props {
		asset: Asset;
		/** The favicon this screen already read out of the journal, when there is one. */
		favicon?: string;
	}

	const { asset, favicon }: Props = $props();

	/**
	 * The identity band, and it replaces the card the asset view used to repeat.
	 *
	 * The first plan was for the detail page to render the same card as the list, from the same
	 * component, so that the display rules lived in one place. What that
	 * produced on screen was the same eight zones twice within four hundred pixels: once
	 * as a row somebody had just clicked, once as the header of the page they landed on.
	 * The asset view keeps the rule and moves it: the card decides how an asset reads in a
	 * **list**, this decides how it reads when it is the subject, and the sentences the
	 * two must never contradict each other on live in $lib/format.
	 */
	const name = $derived(identity(asset));
	const verdict = $derived(verdictOf(asset));

	/** A url is its own address; a service has one only once a probe measured a scheme. */
	const openable = $derived(asset.kind === 'url' ? asset.key : serviceURL(asset));

	/** Everything else on the same host, which is the question this page raises most. */
	const hostHref = $derived(asset.host ? href([{ field: 'host', op: 'eq', value: asset.host }]) : '');
</script>

<section class="ident">
	{#if favicon}
		<!-- In an <img>, the safe container for a value a hostile target produced: an
		     image element executes nothing, even handed an svg. -->
		<img class="fav" src={favicon} alt="" />
	{:else}
		<span class="fav none" aria-hidden="true">?</span>
	{/if}

	<div class="who">
		<h1>
			<span class="key"
				>{name.head}{#if name.path}<span class="path">{name.path}</span>{/if}</span
			>
			{#if openable}
				<!-- The three attributes are not style. This console links to pages that are
				     hostile by hypothesis: `noopener` stops the opened page reaching
				     window.opener and navigating the console's own tab to a fake login,
				     `noreferrer` keeps the console URL out of the target's Referer, and
				     `_blank` leaves this page where it was. -->
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
		</h1>

		<div class="meta">
			<span class="state {asset.lifecycle}">
				<span class="dot"></span>
				<span class="word">{asset.lifecycle}</span>
			</span>
			<span class="tag scope {asset.scope_status}">{asset.scope_status.replace('_', ' ')}</span>
			<span class="tag">{asset.kind}</span>
			{#if asset.title}
				<span class="sep">·</span>
				<span class="title">{asset.title}</span>
			{/if}
			<span class="sep">·</span>
			<span title={exact(asset.first_seen)}>appeared {ago(asset.first_seen)}</span>
			<span class="sep">·</span>
			<span title={exact(asset.last_checked_at)}>
				{asset.last_checked_at ? 'probed ' + ago(asset.last_checked_at) : 'never probed'}
			</span>
			{#if asset.kind === 'service' || asset.kind === 'url'}
				<span class="sep">·</span>
				<span title={exact(asset.last_fingerprint_at)}>
					{asset.last_fingerprint_at ? 'rendered ' + ago(asset.last_fingerprint_at) : 'never rendered'}
				</span>
			{/if}
		</div>
	</div>

	<div class="acts">
		{#if openable}
			<a class="btn" href={openable} target="_blank" rel="noopener noreferrer" referrerpolicy="no-referrer">
				<Icon name="open" />
				Open
			</a>
		{:else}
			<!-- Nothing probes a name over http, and a service that never answered
			     established no scheme. Offering a link built from a guess would be the
			     interface inventing a fact, so it says why there is none. -->
			<span class="btn off" title="No probe has established an address for this asset">No address to open</span>
		{/if}
		{#if hostHref}
			<a class="btn" href={hostHref}>
				<Icon name="search" />
				Everything on this host
			</a>
		{/if}
	</div>
</section>

{#if verdict}
	<p class="why {verdict.tone}">{verdict.text}</p>
{/if}

<style>
	.ident {
		display: flex;
		align-items: flex-start;
		gap: 12px;
		margin-bottom: 12px;
		min-width: 0;
	}

	.fav {
		width: 30px;
		height: 30px;
		border-radius: 5px;
		flex: none;
		margin-top: 2px;
		image-rendering: pixelated;
		background: var(--canvas);
		border: 1px solid var(--border-2);
	}

	.fav.none {
		display: grid;
		place-items: center;
		border-style: dashed;
		border-color: var(--border);
		color: var(--ink-3);
		font-family: var(--font-mono);
		font-size: 12px;
	}

	.who {
		flex: 1;
		min-width: 0;
	}

	h1 {
		margin: 0;
		display: flex;
		align-items: center;
		gap: 8px;
		min-width: 0;
	}

	.key {
		font-family: var(--font-mono);
		font-size: 23px;
		font-weight: 500;
		letter-spacing: -0.015em;
		color: var(--ink);
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.key .path {
		color: var(--ink-3);
	}

	.launch {
		color: var(--ink-3);
		flex: none;
		display: inline-flex;
		padding: 2px;
		border-radius: var(--radius-control);
	}

	.launch:hover {
		color: var(--signal);
		background: var(--card);
	}

	.meta {
		display: flex;
		align-items: center;
		gap: 8px;
		flex-wrap: wrap;
		margin-top: 5px;
		font-size: 12px;
		color: var(--ink-3);
		min-width: 0;
	}

	.meta .title {
		color: var(--ink-2);
		font-weight: 500;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
		max-width: 320px;
	}

	.sep {
		color: var(--border);
	}

	.state {
		display: inline-flex;
		align-items: center;
		gap: 6px;
		flex: none;
	}

	.state .dot {
		width: 8px;
		height: 8px;
		border-radius: 50%;
		background: var(--signal);
	}

	.state .word {
		font-size: 10.5px;
		text-transform: uppercase;
		letter-spacing: 0.06em;
		font-weight: 600;
		color: var(--ink-3);
	}

	.state.flapping .dot {
		background: var(--code-4xx);
	}

	.state.inactive .dot,
	.state.archived .dot {
		background: var(--dead);
	}

	.state.inactive .word,
	.state.archived .word {
		color: var(--dead);
	}

	.state.unobservable .dot {
		background: var(--unobs);
	}

	.state.unobservable .word {
		color: var(--unobs);
	}

	.state.candidate .dot {
		background: transparent;
		border: 2px solid var(--ink-3);
		width: 9px;
		height: 9px;
	}

	.tag {
		font-size: 10.5px;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		font-weight: 600;
		color: var(--ink-3);
		background: var(--card);
		border: 1px solid var(--border);
		border-radius: var(--radius-control);
		padding: 2px 6px;
		flex: none;
	}

	.tag.scope.in_scope {
		color: var(--code-2xx);
		border-color: #c6ecdd;
		background: var(--code-2xx-bg);
	}

	.acts {
		display: flex;
		align-items: center;
		gap: 8px;
		flex: none;
	}

	.btn.off {
		color: var(--ink-3);
		border-style: dashed;
		box-shadow: none;
		cursor: default;
	}

	/* The verdict, never a colour alone. */
	.why {
		margin: 0 0 12px;
		padding: 8px 11px;
		border-radius: var(--radius-control);
		font-size: 12.5px;
		color: var(--ink-2);
		background: var(--unobs-bg);
		border-left: 2px solid var(--unobs);
	}

	.why.dead {
		background: var(--card);
		border-left-color: var(--dead);
	}
</style>
