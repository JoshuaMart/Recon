<script lang="ts">
	import Panel from './Panel.svelte';
	import { daysUntil, elapsed, exact, pivotOf } from '$lib/format';
	import { certificateOf } from '$lib/observation';
	import { badgeFilter, href } from '$lib/query';
	import type { Asset, Evidence } from '$lib/types';

	interface Props {
		asset: Asset;
		evidence?: Evidence;
	}

	const { asset, evidence }: Props = $props();

	const certificate = $derived(certificateOf(evidence));
	const days = $derived(daysUntil(certificate?.notAfter));
	const percent = $derived(elapsed(certificate?.notBefore, certificate?.notAfter));
	const tone = $derived(days === undefined ? '' : days < 0 ? 'gone' : days < 14 ? 'soon' : '');

	/**
	 * The public key pivot, with its counter.
	 *
	 * The pivot hashes the SubjectPublicKeyInfo and not the certificate, so the value survives
	 * renewal: a host that reuses its key keeps the same one. That is what makes it the
	 * pivot for finding an origin behind a CDN, and it is worth the sentence beside the
	 * badge rather than a number nobody can interpret.
	 */
	const pivot = $derived(pivotOf(asset, 'cert_spki'));

	function expiry(): string {
		if (days === undefined) return 'no expiry recorded';
		if (days < 0) return `expired ${-days} ${days === -1 ? 'day' : 'days'} ago`;
		return `${days} ${days === 1 ? 'day' : 'days'} left`;
	}
</script>

<Panel title="Certificate" {evidence} meta="never observed">
	{#if !certificate}
		<p class="dv-note">
			No TLS handshake has completed on this asset, so there is no certificate to show. That is an absence of
			measurement and not a statement about what it presents.
		</p>
	{:else}
		<div class="grid">
			<dl class="dv-kv">
				<dt>Subject</dt>
				<dd>{certificate.subject ?? '—'}</dd>
				<dt>Issuer</dt>
				<dd>{certificate.issuer ?? '—'}</dd>
				<dt>SAN</dt>
				<dd>{certificate.san.length > 0 ? certificate.san.join(', ') : '—'}</dd>
			</dl>
			<dl class="dv-kv">
				<dt>Version</dt>
				<dd>{certificate.version ?? '—'}</dd>
				<dt>Cipher</dt>
				<dd>{certificate.cipher ?? '—'}</dd>
				<dt>Key hash</dt>
				<dd>{certificate.spki ? certificate.spki.slice(0, 24) + '…' : '—'}</dd>
			</dl>
		</div>

		{#if certificate.notBefore || certificate.notAfter}
			<div class="valid">
				<div class="dv-bar {tone}"><i style:width="{percent}%"></i></div>
				<div class="ends">
					<span title={exact(certificate.notBefore)}>issued {certificate.notBefore?.slice(0, 10) ?? '—'}</span>
					<span title={exact(certificate.notAfter)}>
						expires {certificate.notAfter?.slice(0, 10) ?? '—'} · {expiry()}
					</span>
				</div>
			</div>
		{/if}

		{#if pivot}
			<div class="dv-divide"></div>
			<div class="dv-row">
				<span class="dv-lbl">Pivot</span>
				<a class="dv-badge hash" href={href([badgeFilter('cert_spki', pivot.value)])}>
					<span class="k">cert</span>
					<span class="v">{pivot.value.slice(0, 8)}</span>
					<span class="n">{pivot.count}</span>
				</a>
				<span class="says">
					{#if pivot.count > 1}
						{pivot.count - 1}
						{pivot.count === 2 ? 'other asset presents' : 'other assets present'} this public key. It is the join no name
						would have made.
					{:else}
						No other asset presents this public key.
					{/if}
				</span>
			</div>
		{/if}
	{/if}
</Panel>

<style>
	.grid {
		display: grid;
		grid-template-columns: repeat(auto-fit, minmax(300px, 1fr));
		gap: 12px 28px;
	}

	.valid {
		margin-top: 14px;
	}

	.ends {
		display: flex;
		justify-content: space-between;
		gap: 12px;
		font-size: 10.5px;
		color: var(--ink-3);
		font-family: var(--font-mono);
		margin-top: 5px;
	}

	.says {
		font-size: 11.5px;
		color: var(--ink-3);
	}
</style>
