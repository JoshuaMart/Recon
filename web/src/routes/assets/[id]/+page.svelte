<script lang="ts">
	import '$lib/detail.css';
	import AssetHeader from '$lib/components/AssetHeader.svelte';
	import CertificatePanel from '$lib/components/CertificatePanel.svelte';
	import FactStrip from '$lib/components/FactStrip.svelte';
	import HttpPanel from '$lib/components/HttpPanel.svelte';
	import Lineage from '$lib/components/Lineage.svelte';
	import PortsPanel from '$lib/components/PortsPanel.svelte';
	import RawEvidence from '$lib/components/RawEvidence.svelte';
	import RenderPanel from '$lib/components/RenderPanel.svelte';
	import Timeline from '$lib/components/Timeline.svelte';
	import { faviconOf } from '$lib/evidence';
	import { ago, exact, webSurface } from '$lib/format';
	import { layerOf } from '$lib/observation';
	import type { Evidence } from '$lib/types';
	import type { PageData } from './$types';

	const { data }: { data: PageData } = $props();

	const asset = $derived(data.detail.asset);
	/** Free on this screen: it already reads the journal, so the bytes are in hand. */
	const favicon = $derived(faviconOf(data.detail.evidence));

	/**
	 * The layers, read once and handed to the panels that own them.
	 *
	 * Each panel reads the last observation of one layer and states what it
	 * measured, in the vocabulary of that layer rather than in the vocabulary of a JSON
	 * walk. The walk is still there, whole, at the bottom.
	 */
	const http = $derived(layerOf(data.detail.evidence, 'http'));
	const tcp = $derived(layerOf(data.detail.evidence, 'tcp'));
	const render = $derived(layerOf(data.detail.evidence, 'fingerprint'));

	/**
	 * The layers this asset has never had an observation of.
	 *
	 * Named rather than omitted, and only where the layer would apply: nothing probes
	 * a name over http, so "no fingerprint" on an fqdn is a rule and not a gap, and saying
	 * it would answer a question nobody asked.
	 */
	const never = $derived.by(() => {
		const expected: Evidence['layer'][] = webSurface(asset) ? ['dns', 'tcp', 'http', 'fingerprint'] : ['dns', 'tcp'];
		const seen = new Set(data.detail.evidence.map((entry) => entry.layer));
		return expected.filter((layer) => !seen.has(layer));
	});
</script>

<svelte:head><title>{asset.key} · recon</title></svelte:head>

<div class="detail">
	<nav class="back">
		<a href="/">← Back to the inventory</a>
	</nav>

	<AssetHeader {asset} {favicon} />

	<FactStrip {asset} {http} {tcp} enriched={data.enriched} />

	<div class="columns">
		<div class="main">
			<!-- The http layers when the kind is a web surface, and also when it is not but
			     an observation exists anyway: nothing probes a name over http, and the
			     observations taken before it stopped are still measurements. Hiding a panel
			     over data the fold below shows would be the interface deciding what counts. -->
			{#if webSurface(asset) || http}
				<HttpPanel {asset} evidence={http} />
				<CertificatePanel {asset} evidence={http} />
			{/if}
			{#if webSurface(asset) || render}
				<RenderPanel {asset} evidence={render} {favicon} />
			{/if}
			<PortsPanel {asset} evidence={tcp} />
			<Timeline timeline={data.detail.timeline} truncated={data.detail.truncated_layers ?? []} />
			<RawEvidence evidence={data.detail.evidence} {never} />
		</div>

		<aside class="side">
			<Lineage {asset} />

			<section class="dv-panel facts">
				<h2>Identity</h2>
				<dl class="dv-kv">
					<dt>Kind</dt>
					<dd class="plain">{asset.kind}</dd>
					{#if asset.host}
						<dt>Host</dt>
						<dd>{asset.host}</dd>
					{/if}
					{#if asset.port}
						<dt>Port</dt>
						<dd>{asset.port}/tcp</dd>
					{/if}
					<dt>Scheme</dt>
					<!-- The scheme is the one the probe measured, never one inferred from
					     the port. Its absence is exact rather than prudent. -->
					<dd class:none={!asset.scheme}>{asset.scheme ?? 'none measured'}</dd>
					<!-- The canonical key, and not the readable spelling the header
					     carries: `vulns.jomar.ovh:80/tcp` is what the database holds and what an
					     export or an API call needs. -->
					<dt>Key</dt>
					<dd>{asset.key}</dd>
					<dt>Scope</dt>
					<dd class="plain">{asset.scope_status.replace('_', ' ')}</dd>
					<dt>Lifecycle</dt>
					<dd class="plain">{asset.lifecycle}</dd>
				</dl>

				<div class="dv-divide"></div>
				<div class="dv-lbl layers">Layers</div>
				<div class="dv-row">
					{#each [['dns', asset.dns_state], ['tcp', asset.tcp_state], ['http', asset.http_state]] as [layer, state] (layer)}
						{#if state}
							<span class="dv-badge plain"><span class="k">{layer}</span><span class="v">{state}</span></span>
						{:else}
							<span class="dv-badge absent">{layer} not measured</span>
						{/if}
					{/each}
				</div>
			</section>

			<section class="dv-panel facts">
				<h2>Dates</h2>
				<dl class="dv-kv wide">
					<dt>First seen</dt>
					<dd class="plain" title={exact(asset.first_seen)}>{ago(asset.first_seen)}</dd>
					<dt>Last seen</dt>
					<dd class="plain" title={exact(asset.last_seen)}>{ago(asset.last_seen)}</dd>
					<dt>Last probed</dt>
					<dd class="plain" title={exact(asset.last_checked_at)}>{ago(asset.last_checked_at)}</dd>
					<dt>Last changed</dt>
					<dd class="plain" title={exact(asset.last_changed_at)}>{ago(asset.last_changed_at)}</dd>
					{#if webSurface(asset)}
						<!-- The fingerprinter runs on five triggers rather than on a cadence,
						     so its clock is separate and the gap is the point. -->
						<dt>Last rendered</dt>
						<dd class="plain" title={exact(asset.last_fingerprint_at)}>{ago(asset.last_fingerprint_at)}</dd>
					{/if}
				</dl>
			</section>

			{#if !data.enriched}
				<!-- the three absences, first state: a deployment with no MaxMind database is a normal
				     deployment, so the infrastructure family is not shown at all. The absence
				     is said once, where it can be acted on, rather than as an empty block. -->
				<p class="settings">This deployment derives no ASN and no geolocation.</p>
			{/if}

			<p class="window">History read from {exact(data.detail.window_from)}.</p>
		</aside>
	</div>
</div>

<style>
	.detail {
		padding: 14px 18px 60px;
		min-width: 0;
	}

	.back {
		margin-bottom: 10px;
	}

	.back a {
		font-size: 12px;
		color: var(--ink-3);
		text-decoration: none;
	}

	.back a:hover {
		color: var(--ink);
	}

	.columns {
		display: grid;
		grid-template-columns: minmax(0, 1fr) 300px;
		gap: 12px;
		align-items: start;
	}

	@media (max-width: 1000px) {
		.columns {
			grid-template-columns: minmax(0, 1fr);
		}
	}

	.main,
	.side {
		display: grid;
		gap: 12px;
		min-width: 0;
	}

	.facts {
		padding: 14px 16px 15px;
	}

	h2 {
		font-size: 11px;
		text-transform: uppercase;
		letter-spacing: 0.07em;
		color: var(--ink-3);
		font-weight: 600;
		margin: 0 0 11px;
	}

	.layers {
		margin-bottom: 7px;
	}

	.dv-kv.wide {
		grid-template-columns: 96px minmax(0, 1fr);
	}

	.settings {
		margin: 0;
		font-size: 11px;
		color: var(--ink-3);
		border: 1px dashed var(--border);
		border-radius: var(--radius-card);
		padding: 10px 12px;
	}

	.window {
		margin: 0;
		font-size: 11px;
		color: var(--ink-3);
		text-align: right;
	}
</style>
