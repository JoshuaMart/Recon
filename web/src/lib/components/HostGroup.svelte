<script lang="ts">
	import AssetRow from './AssetRow.svelte';
	import Icon from './Icon.svelte';
	import { ago, badgesOf, exact, flag } from '$lib/format';
	import { frontDoorFavicon, sharedOf, splitGroup } from '$lib/group';
	import { href, withFilter, type Filter } from '$lib/query';
	import type { Group } from '$lib/types';

	interface Props {
		group: Group;
		filters: Filter[];
		/** Whether this deployment derives ASN and geolocation at all (the three absences). */
		enriched: boolean;
		favicons: Record<string, string>;
	}

	const { group, filters, enriched, favicons }: Props = $props();

	/**
	 * The host, and what its services agree on (the fold).
	 *
	 * The header states the operator, the address, the certificate and the lineage once
	 * instead of once per service — but only where `sharedOf` found the members to agree,
	 * because `Group` carries no aggregate and a header that guesses is a header nobody
	 * can check. Anything they disagree on stays on the rows, where it is true.
	 */
	const { self, services } = $derived(splitGroup(group));
	const shared = $derived(sharedOf(group));

	/**
	 * The icon, which answers a different question from the tags beside it.
	 *
	 * A tag **claims** something about the host and only rises on agreement. An image
	 * **shows** what the name looks like, and a host serving four applications was
	 * getting an empty square — which reads as a missing favicon rather than as four
	 * different ones. So the icon falls back to the front door, and it is a pivot with
	 * its counter only where the whole host agrees, since that is the only case where
	 * the count describes the host rather than one of its services.
	 */
	const iconHash = $derived(shared.favicon ?? frontDoorFavicon(group));
	const icon = $derived(iconHash ? favicons[iconHash] : undefined);
	const iconBadge = $derived(
		shared.favicon
			? group.assets
					.flatMap((asset) => badgesOf(asset))
					.find((pivot) => pivot.type === 'favicon' && pivot.value === shared.favicon)
			: undefined
	);

	/** On a fronted asset the address is a point of presence, so the city would say where the CDN is. */
	const geo = $derived(
		!shared.isCdn && shared.country ? `${flag(shared.country)} ${shared.city ?? shared.country}` : ''
	);

	const hostHref = $derived(href(withFilter(filters, { field: 'host', op: 'eq', value: group.host })));
</script>

<section class="group">
	<header>
		{#if icon && iconBadge}
			<!-- Shared by every service, so the image is the pivot and carries its count.
			     In an <img>, the safe container for a value a hostile target produced. -->
			<a
				class="favlink"
				href={href(withFilter(filters, { field: 'favicon_hash', op: 'eq', value: shared.favicon ?? '' }))}
				title="{iconBadge.count - 1} other assets share this favicon"
			>
				<img class="fav" src={icon} alt="" />
			</a>
		{:else if icon}
			<img
				class="fav"
				src={icon}
				alt=""
				title="The favicon of the front door of this host. Its services do not all serve the same one, so each row carries its own."
			/>
		{:else}
			<span class="fav none" aria-hidden="true"></span>
		{/if}

		{#if self}
			<!-- The header is the asset when the host is one of ours, so opening it opens
			     the fqdn rather than a filtered list of its services. It carries a status
			     code that repeats its own :443, which is why it is not a row of its own. -->
			<a class="host" href="/assets/{self.asset_id}">{group.host}</a>
		{:else}
			<span class="host">{group.host}</span>
		{/if}

		<a class="count" href={hostHref} title="Filter the list to this host">
			{services.length === 1 ? '1 service' : services.length + ' services'}
		</a>

		<span class="spacer"></span>

		<div class="shared">
			{#if enriched && shared.asn}
				<a class="tag" href={href(withFilter(filters, { field: 'asn', op: 'eq', value: String(shared.asn) }))}>
					<span class="v">AS{shared.asn}</span>{shared.asnOrg ?? ''}
				</a>
			{/if}
			{#if enriched && geo}
				<a class="tag" href={href(withFilter(filters, { field: 'country', op: 'eq', value: shared.country ?? '' }))}>
					{geo}
				</a>
			{/if}
			{#if enriched && shared.isCdn}
				<span class="tag cdn" title="No geolocation on a fronted asset: the address is that of a point of presence .">
					CDN {shared.cdnProvider ?? ''}
				</span>
			{/if}
			{#if shared.ip}
				<span class="tag"><span class="v">{shared.ip}</span></span>
			{/if}
			{#if shared.cert}
				<a
					class="tag cert"
					href={href(withFilter(filters, { field: 'cert_spki_hash', op: 'eq', value: shared.cert.value }))}
					title="{shared.cert.count - 1} other assets present this public key"
				>
					cert <span class="v">{shared.cert.value.slice(0, 8)}</span><span class="n">{shared.cert.count}</span>
				</a>
			{/if}
		</div>

		<time title={exact(group.last_seen)}>{ago(group.last_seen)}</time>
	</header>

	<div class="rows">
		{#each services as asset (asset.asset_id)}
			<AssetRow {asset} {filters} {shared} />
		{/each}
		{#if services.length === 0 && self}
			<!-- A name with no service of its own. One line rather than a header over
			     nothing: seventeen of the thirty-three groups of a real inventory are
			     exactly this, and a card each was half a screen of empty zones. -->
			<AssetRow asset={self} {filters} {shared} withHost />
		{/if}
	</div>

	{#if shared.lineage}
		<!-- Once per host rather than once per service. It was the same sentence on six
		     consecutive rows, and it only rises here when every service agrees on it
		     (the lineage is per asset, and two services of one host can differ). -->
		<p class="lineage" title={shared.lineage}>
			<Icon name="lineage" />
			<span>{shared.lineage}</span>
		</p>
	{/if}
</section>

<style>
	.group {
		background: var(--card);
		border: 1px solid var(--border);
		border-radius: var(--radius-card);
		box-shadow: var(--card-shadow);
		margin-bottom: 8px;
		overflow: hidden;
	}

	header {
		display: flex;
		align-items: center;
		gap: 9px;
		padding: 8px 12px 8px 11px;
		min-width: 0;
	}

	.fav {
		width: 17px;
		height: 17px;
		border-radius: 4px;
		flex: none;
		image-rendering: pixelated;
	}

	.fav.none {
		border: 1px dashed var(--border);
		background: var(--canvas);
	}

	.favlink {
		display: inline-flex;
		flex: none;
		border-radius: 4px;
	}

	.favlink:hover {
		box-shadow: 0 0 0 2px var(--signal-bg);
	}

	.host {
		font-family: var(--font-mono);
		font-size: 13.5px;
		font-weight: 600;
		color: var(--ink);
		text-decoration: none;
		flex: none;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
		max-width: 340px;
	}

	a.host {
		text-decoration: underline;
		text-decoration-color: var(--border-2);
		text-underline-offset: 3px;
	}

	a.host:hover {
		text-decoration-color: var(--signal);
	}

	.count {
		font-size: 11.5px;
		color: var(--ink-3);
		text-decoration: none;
		flex: none;
	}

	.count:hover {
		color: var(--ink);
	}

	.shared {
		display: flex;
		align-items: center;
		gap: 5px;
		min-width: 0;
		overflow: hidden;
	}

	.tag {
		display: inline-flex;
		align-items: center;
		gap: 5px;
		font-size: 11px;
		color: var(--ink-3);
		border: 1px solid var(--border-2);
		border-radius: var(--radius-control);
		padding: 1px 6px;
		white-space: nowrap;
		text-decoration: none;
		min-width: 0;
		overflow: hidden;
		text-overflow: ellipsis;
	}

	a.tag:hover {
		border-color: var(--ink-3);
	}

	.tag .v {
		font-family: var(--font-mono);
		color: var(--ink-2);
	}

	.tag .n {
		font-family: var(--font-mono);
		font-size: 10px;
		font-weight: 600;
	}

	.tag.cert {
		background: var(--signal-bg);
		border-color: transparent;
		color: #0a7a58;
	}

	.tag.cert .v {
		color: #0a7a58;
	}

	.tag.cdn {
		background: var(--cdn-bg);
		border-color: transparent;
		color: var(--cdn);
	}

	time {
		font-size: 11.5px;
		color: var(--ink-3);
		flex: none;
	}

	.rows {
		border-top: 1px solid var(--border-2);
	}

	.rows :global(.row:first-child) {
		border-top: 0;
	}

	.lineage {
		display: flex;
		align-items: center;
		gap: 6px;
		margin: 0;
		padding: 5px 12px 6px 11px;
		background: var(--temporal-bg);
		border-top: 1px solid var(--border-2);
		font-size: 11px;
		color: var(--ink-3);
		min-width: 0;
	}

	.lineage span {
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.lineage :global(svg) {
		width: 12px;
		height: 12px;
	}
</style>
