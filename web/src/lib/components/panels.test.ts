import { describe, expect, it } from 'vitest';
import { render } from 'svelte/server';
import FactStrip from './FactStrip.svelte';
import HttpPanel from './HttpPanel.svelte';
import RawEvidence from './RawEvidence.svelte';
import RenderPanel from './RenderPanel.svelte';
import type { Asset, Evidence } from '$lib/types';

/**
 * The panels of the asset view, rendered on the server.
 *
 * These are not screenshot tests and they do not check a layout. What they hold is the
 * one property the whole redesign rests on: a panel with nothing to show must say which
 * nothing it is. That sentence is the thing a refactor silently loses, and it is exactly
 * what the flat evidence dump could never say.
 */

function asset(overrides: Partial<Asset> = {}): Asset {
	return {
		asset_id: '00000000-0000-0000-0000-000000000001',
		program_id: '00000000-0000-0000-0000-0000000000ff',
		discovery_source: 'fastrecon',
		kind: 'service',
		key: 'vulns.jomar.ovh:80/tcp',
		host: 'vulns.jomar.ovh',
		port: 80,
		scheme: 'http',
		scope_status: 'in_scope',
		lifecycle: 'active',
		technologies: [],
		attributes: {},
		volatility: 3,
		first_seen: '2026-08-18T09:00:00Z',
		last_seen: '2026-08-18T11:00:00Z',
		last_checked_at: '2026-08-18T11:00:00Z',
		...overrides
	};
}

function observation(layer: Evidence['layer'], data: unknown, overrides: Partial<Evidence> = {}): Evidence {
	return {
		layer,
		outcome: 'ok',
		source: layer + '-check',
		observed_at: '2026-08-18T09:00:00Z',
		last_confirmed_at: '2026-08-18T11:00:00Z',
		data: data as Record<string, unknown>,
		...overrides
	};
}

const httpPayload = {
	url: 'http://vulns.jomar.ovh:80',
	final_url: 'https://vulns.jomar.ovh/',
	status_code: 200,
	headers: { 'content-type': 'text/html; charset=utf-8', 'strict-transport-security': 'max-age=31536000' },
	chain: [
		{ code: 301, url: 'http://vulns.jomar.ovh:80', headers: { location: 'https://vulns.jomar.ovh/' } },
		{
			code: 200,
			url: 'https://vulns.jomar.ovh/',
			headers: { 'content-type': 'text/html; charset=utf-8', 'strict-transport-security': 'max-age=31536000' }
		}
	],
	cookie_names: ['_lighthouse_session'],
	tls: {
		version: 'TLS 1.3',
		issuer: "CN=YR2, O=Let's Encrypt, C=US",
		not_before: '2026-08-17T10:30:58Z',
		not_after: '2026-11-15T10:30:57Z',
		cert_spki_hash: '561d31de66be9d718f6b7f6f325b5afd'
	}
};

describe('HttpPanel', () => {
	it('names the security headers that were not sent', () => {
		const { body } = render(HttpPanel, {
			props: {
				asset: asset({ status_code: 200, status_chain: [301, 200] }),
				evidence: observation('http', httpPayload)
			}
		});
		expect(body).toContain('Strict-Transport-Security');
		// The discriminating half: an absent header is named, which is the whole reason
		// this block is the console's one allowlist.
		expect(body).toContain('Content-Security-Policy');
		expect(body).toContain('not sent');
		expect(body).toContain('1 of 8 present');
	});

	it('draws every hop of the chain and not only the code that answered', () => {
		const { body } = render(HttpPanel, {
			props: {
				asset: asset({ status_code: 200, status_chain: [301, 200] }),
				evidence: observation('http', httpPayload)
			}
		});
		expect(body).toContain('301');
		expect(body).toContain('https://vulns.jomar.ovh/');
	});

	// An asset in the protected regime obtains nothing usable, and four absences of
	// measurement must not read as four measurements.
	it('says what was asked and what came back when nothing answered', () => {
		const { body } = render(HttpPanel, {
			props: {
				asset: asset({ lifecycle: 'unobservable', http_state: 'dead' }),
				evidence: observation('http', { error: 'tls: handshake failure' }, { outcome: 'fail' })
			}
		});
		expect(body).toContain('tls: handshake failure');
		expect(body).toContain('no status code');
		expect(body).not.toContain('present');
	});
});

describe('RenderPanel', () => {
	it('separates a render with nothing to show from an asset never rendered', () => {
		const never = render(RenderPanel, { props: { asset: asset(), evidence: undefined } });
		expect(never.body).toContain('never rendered');
		expect(never.body).toContain('triggered by a usable http response');

		const rendered = render(RenderPanel, {
			props: { asset: asset(), evidence: observation('fingerprint', { metadata: { robots_txt: true }, cookies: {} }) }
		});
		expect(rendered.body).toContain('no technology recognised');
		expect(rendered.body).not.toContain('triggered by a usable http response');
	});
});

describe('FactStrip', () => {
	it('shows no network tile at all when the deployment does not enrich', () => {
		const bare = render(FactStrip, {
			props: { asset: asset(), http: observation('http', httpPayload), tcp: undefined, enriched: false }
		});
		expect(bare.body).not.toContain('Network');

		// the three absences's third state: it enriches and this address matched nothing, which is a
		// different sentence from "not configured" and must be said rather than left blank.
		const enriched = render(FactStrip, {
			props: { asset: asset(), http: observation('http', httpPayload), tcp: undefined, enriched: true }
		});
		expect(enriched.body).toContain('Network');
		expect(enriched.body).toContain('no match');
	});

	it('reports the scan width, which is what says a scan ran', () => {
		const { body } = render(FactStrip, {
			props: {
				asset: asset(),
				http: undefined,
				tcp: observation('tcp', { open_ports: [80], scanned_ports: Array.from({ length: 100 }, (_, i) => i) }),
				enriched: false
			}
		});
		expect(body).toContain('one open of 100 scanned');
	});
});

describe('RawEvidence', () => {
	it('names a layer that has never been observed instead of omitting it', () => {
		const { body } = render(RawEvidence, {
			props: { evidence: [observation('http', httpPayload)], never: ['fingerprint'] }
		});
		expect(body).toContain('no observation of this layer, ever');
		// Folded, so the payload is not what the page opens with.
		expect(body).toContain('Show');
		expect(body).not.toContain('561d31de66be9d718f6b7f6f325b5afd');
	});
});
