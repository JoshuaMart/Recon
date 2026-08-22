<script lang="ts">
	import { ago, codeFamily, daysUntil, elapsed, exact, flag, geoVisible, webSurface } from '$lib/format';
	import { certificateOf, portsOf } from '$lib/observation';
	import type { Asset, Evidence } from '$lib/types';

	interface Props {
		asset: Asset;
		http?: Evidence;
		tcp?: Evidence;
		/** Whether this deployment derives ASN and geolocation at all (the three absences). */
		enriched: boolean;
	}

	const { asset, http, tcp, enriched }: Props = $props();

	interface Tile {
		label: string;
		value: string;
		/** Set when the value is a status code, so it takes its family's colour. */
		family?: string;
		/** Set when the value is an absence rather than a measurement. */
		none?: boolean;
		unit?: string;
		sub: string;
		bar?: { percent: number; tone: string };
	}

	/**
	 * The six answers, and which of them this asset can give.
	 *
	 * This is the "general information" block every tool of this kind opens with, and the
	 * rule it follows here is the three absences's: a tile that has nothing to say says which
	 * nothing it is, and a family this deployment cannot compute is **not shown at all**.
	 * The network tile is the one that disappears — a deployment with no MaxMind database
	 * is a normal deployment , and an empty tile reads as a broken interface.
	 */
	const tiles = $derived.by(() => {
		const out: Tile[] = [];
		const chain = asset.status_chain ?? [];
		const certificate = certificateOf(http);
		const ports = portsOf(tcp);

		if (webSurface(asset)) {
			out.push(response(chain));
			out.push(certificateTile(certificate));
		} else {
			// The layer verdict, in the vocabulary the layer uses. `unmeasured` and
			// `dead` are two different absences, and collapsing them into "unknown"
			// would lose the one that is a measurement.
			out.push({
				label: 'Name',
				value: asset.dns_state ?? 'unmeasured',
				none: asset.dns_state !== 'healthy',
				sub: asset.dns_state === 'healthy' ? 'the name resolves' : 'nothing else is asked of a name'
			});
		}

		if (enriched) out.push(network());

		if (ports.open.length > 0 || asset.port) {
			const open = ports.open.length > 0 ? ports.open : asset.port ? [asset.port] : [];
			out.push({
				label: 'Open ports',
				value: open.join(', '),
				unit: '/tcp',
				// A service is scanned on its own port and nothing else, so "one open of 1
				// scanned" would be a sentence about nothing. The scan width is only worth
				// saying when there was a width.
				sub: !ports.scanned
					? 'from the key of this asset, never scanned'
					: ports.scanned > open.length
						? `${open.length === 1 ? 'one open' : open.length + ' open'} of ${ports.scanned} scanned`
						: 'the port this asset is, and it answers'
			});
		}

		out.push(probe());
		out.push({
			label: 'Volatility',
			value: String(asset.volatility),
			unit: asset.volatility === 1 ? 'change' : 'changes',
			// No threshold colour, and that is milestone 7's "no composite score, no
			// severity" rather than restraint: the bands the volatility rule wants need weeks of real
			// data, and painting the number would be inventing them.
			sub: 'over the last 7 days'
		});
		return out;
	});

	function response(chain: number[]): Tile {
		const hops = chain.length > 1 ? 'via ' + chain.slice(0, -1).join(', ') + ' · ' : '';
		if (asset.status_code) {
			return {
				label: 'Response',
				value: String(asset.status_code),
				family: codeFamily(asset.status_code),
				unit: chain.length > 1 ? `after ${chain.length - 1} ${chain.length === 2 ? 'hop' : 'hops'}` : undefined,
				sub: asset.final_url ? `${hops}lands on ${asset.final_url}` : hops ? hops.slice(0, -3) : 'no redirect'
			};
		}
		if (!asset.last_checked_at) {
			return { label: 'Response', value: 'never probed', none: true, sub: 'no http observation yet' };
		}
		return {
			label: 'Response',
			value: 'no answer',
			none: true,
			sub: asset.http_state ? 'http ' + asset.http_state : 'the probe obtained nothing'
		};
	}

	function certificateTile(certificate: ReturnType<typeof certificateOf>): Tile {
		if (!certificate) {
			return { label: 'Certificate', value: 'never seen', none: true, sub: 'no handshake ever completed' };
		}
		const days = daysUntil(certificate.notAfter);
		const issuer = shortIssuer(certificate.issuer);
		const left =
			days === undefined
				? 'no expiry recorded'
				: days < 0
					? `expired ${-days} ${days === -1 ? 'day' : 'days'} ago`
					: `${days} ${days === 1 ? 'day' : 'days'} left`;
		return {
			label: 'Certificate',
			value: certificate.version ?? 'TLS',
			sub: issuer ? `${issuer} · ${left}` : left,
			bar: {
				percent: elapsed(certificate.notBefore, certificate.notAfter),
				tone: days === undefined ? '' : days < 0 ? 'gone' : days < 14 ? 'soon' : ''
			}
		};
	}

	function network(): Tile {
		if (!asset.asn && !asset.country) {
			return {
				label: 'Network',
				value: 'no match',
				none: true,
				sub: 'this deployment enriches, and this address is in neither database'
			};
		}
		const place = geoVisible(asset) ? `${flag(asset.country)} ${asset.city ?? asset.country}` : '';
		const parts = [asset.asn_org, place, asset.ip].filter(Boolean);
		return {
			label: 'Network',
			value: asset.asn ? 'AS' + asset.asn : (asset.country ?? ''),
			// On a fronted target the address is that of a point of presence, so the
			// city would name where the CDN is and not where the asset is.
			sub: asset.is_cdn
				? `CDN ${asset.cdn_provider ?? ''} · ${asset.asn_org ?? 'no geolocation on a fronted asset'}`
				: parts.join(' · ')
		};
	}

	function probe(): Tile {
		const rendered = asset.last_fingerprint_at;
		return {
			label: 'Last probe',
			value: asset.last_checked_at ? ago(asset.last_checked_at) : 'never',
			none: !asset.last_checked_at,
			// The fingerprinter runs on triggers rather than on a cadence , so the gap
			// between the two clocks is a fact about the data and not a delay to fix.
			sub: webSurface(asset) ? (rendered ? 'rendered ' + ago(rendered) : 'never rendered') : 'dns and tcp only'
		};
	}

	/** `CN=YR2, O=Let's Encrypt, C=US` is the authority's name plus three labels nobody reads. */
	function shortIssuer(issuer: string | undefined): string {
		if (!issuer) return '';
		const organisation = /O=([^,]+)/.exec(issuer);
		return (organisation?.[1] ?? issuer).trim();
	}
</script>

<div class="strip">
	{#each tiles as tile (tile.label)}
		<div class="tile">
			<div class="lbl">{tile.label}</div>
			<div class="value">
				<span class="v" class:none={tile.none} data-code={tile.family ?? ''}>{tile.value}</span>
				{#if tile.unit}<span class="unit">{tile.unit}</span>{/if}
			</div>
			<div class="sub" title={tile.label === 'Last probe' ? exact(asset.last_checked_at) : ''}>{tile.sub}</div>
			{#if tile.bar}
				<div class="dv-bar {tile.bar.tone}" style="margin-top: 7px"><i style:width="{tile.bar.percent}%"></i></div>
			{/if}
		</div>
	{/each}
</div>

<style>
	.strip {
		display: grid;
		grid-template-columns: repeat(auto-fit, minmax(196px, 1fr));
		gap: 10px;
		margin-bottom: 12px;
	}

	.tile {
		background: var(--card);
		border: 1px solid var(--border);
		border-radius: var(--radius-card);
		box-shadow: var(--card-shadow);
		padding: 9px 12px 10px;
		min-width: 0;
		overflow: hidden;
	}

	.lbl {
		font-size: 9.5px;
		text-transform: uppercase;
		letter-spacing: 0.08em;
		color: var(--ink-3);
		font-weight: 600;
	}

	.value {
		display: flex;
		align-items: baseline;
		gap: 6px;
		margin-top: 5px;
		min-width: 0;
		overflow: hidden;
	}

	.v {
		font-family: var(--font-mono);
		font-size: 17px;
		font-weight: 600;
		color: var(--ink);
		border-radius: var(--radius-control);
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	/* An absence is not a number, so it is not set in the numeric face. */
	.v.none {
		font-family: var(--font-sans);
		font-size: 13.5px;
		font-weight: 500;
		font-style: italic;
		color: var(--ink-3);
	}

	.v[data-code='2xx'] {
		color: var(--code-2xx);
		background: var(--code-2xx-bg);
		font-size: 15px;
		padding: 2px 7px;
	}

	.v[data-code='3xx'] {
		color: var(--code-3xx);
		background: var(--code-3xx-bg);
		font-size: 15px;
		padding: 2px 7px;
	}

	.v[data-code='4xx'] {
		color: var(--code-4xx);
		background: var(--code-4xx-bg);
		font-size: 15px;
		padding: 2px 7px;
	}

	.v[data-code='5xx'] {
		color: var(--code-5xx);
		background: var(--code-5xx-bg);
		font-size: 15px;
		padding: 2px 7px;
	}

	.unit {
		font-size: 11.5px;
		font-weight: 500;
		color: var(--ink-3);
	}

	/* The subtitle carries the URL and the address, so it wraps rather than being cut:
	   truncating it removes exactly what the tile exists to show. */
	.sub {
		font-size: 11px;
		color: var(--ink-3);
		margin-top: 3px;
		line-height: 1.35;
		overflow-wrap: anywhere;
	}
</style>
