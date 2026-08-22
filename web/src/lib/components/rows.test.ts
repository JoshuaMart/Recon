import { describe, expect, it } from 'vitest';
import { render } from 'svelte/server';
import AssetRow from './AssetRow.svelte';
import HostGroup from './HostGroup.svelte';
import Timeline from './Timeline.svelte';
import type { Asset, Group } from '$lib/types';

/**
 * The row of the fold, rendered on the server.
 *
 * The card carried four milestone 7 assertions and the row inherits them, which is the
 * whole risk of replacing it: a shape that loses the explicit mention of an
 * `unobservable`, or the three states of the cookie badge, passes every other test in
 * this suite. These hold them on the new shape.
 */

function asset(overrides: Partial<Asset> = {}): Asset {
	return {
		asset_id: '00000000-0000-0000-0000-000000000001',
		program_id: '00000000-0000-0000-0000-0000000000ff',
		discovery_source: 'fastrecon',
		kind: 'service',
		key: 'a.acme.test:443/tcp',
		host: 'a.acme.test',
		port: 443,
		scheme: 'https',
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

const row = (a: Asset, props: Record<string, unknown> = {}) =>
	render(AssetRow, { props: { asset: a, filters: [], ...props } }).body;

describe('AssetRow', () => {
	// An unobservable asset needs an explicit mention and not a colour alone, and
	// the three absences says why the two sentences differ in kind. The row has no band, so the
	// sentence takes the cell of a title an unreachable asset does not have.
	it('writes the sentence of an unobservable asset, not only its colour', () => {
		const body = row(asset({ lifecycle: 'unobservable', http_state: 'dead' }));
		expect(body).toContain('No observer gets through');
		expect(body).toContain('dot unobservable');
	});

	it('separates an inactive asset from an unobservable one in words', () => {
		const body = row(asset({ lifecycle: 'inactive', dns_state: 'dead' }));
		expect(body).toContain('no longer resolves');
		expect(body).not.toContain('No observer gets through');
	});

	// Nothing probes a name over http, so "no answer" would answer a question
	// nobody asked, and "never rendered" would read as a pending state rather than a rule.
	it('measures a name on its dns state and says nothing about a render', () => {
		const body = row(
			asset({ kind: 'fqdn', key: 'a.acme.test', port: undefined, scheme: undefined, dns_state: 'healthy' })
		);
		expect(body).toContain('dns healthy');
		expect(body).not.toContain('no answer');
		expect(body).not.toContain('never rendered');
	});

	// The three states of the three absences, which milestone 7 asserts on two of them and the three absences
	// added the third: rendered, cookies, and both display filters removed all of them.
	it('says which absence a missing cookie badge is', () => {
		expect(row(asset())).toContain('never rendered');
		expect(row(asset({ last_fingerprint_at: '2026-08-18T08:00:00Z' }))).toContain('no cookie');
		const filtered = asset({
			last_fingerprint_at: '2026-08-18T08:00:00Z',
			attributes: { cookie_names: ['PHPSESSID'] }
		});
		expect(row(filtered)).toContain('none worth a badge');
	});

	// the three absences took script hashes off the card on a granularity measurement, and the row
	// is the same line at a smaller scale.
	// A badge that says something and does nothing is a fact somebody has to
	// retype into the filter bar this interface deliberately does not have.
	it('makes every pivot badge a link that re-runs the search', () => {
		const body = row(
			asset({
				last_fingerprint_at: '2026-08-18T10:00:00Z',
				pivots: [
					{ type: 'cookie_name', value: 'SESS_INTERNAL', count: 4, badge: true },
					{ type: 'favicon', value: 'abc123def', count: 9, badge: true }
				]
			})
		);
		// The field the server searches, not the name the pivot goes by: the two
		// vocabularies differ, and a value keyed on the wrong one matches nothing
		// rather than failing.
		expect(body).toContain('href="/?f=cookie_name%3Acontains%3ASESS_INTERNAL"');
		expect(body).toContain('href="/?f=favicon_hash%3Aeq%3Aabc123def"');
		// And the counter travels with it, because that is what turns an attribute
		// into a lead.
		expect(body).toContain('>4<');
		expect(body).toContain('>9<');
	});

	// The badge decision is the server's and this never re-makes it. A second
	// copy of the denylist here would be a second list to keep in step, and the
	// divergence would read as a badge appearing on one screen and not the other.
	it('draws no badge for a pivot the server did not mark', () => {
		const body = row(
			asset({
				last_fingerprint_at: '2026-08-18T10:00:00Z',
				// Both halves, because that is what the wire carries: the pivot with
				// the server's verdict on it, and the attribute the value came from.
				// The second is what separates "sets no cookie" from "sets one no
				// badge deserves", and reading only the pivots would state the first
				// where the second is true.
				attributes: { cookie_names: ['PHPSESSID'] },
				pivots: [{ type: 'cookie_name', value: 'PHPSESSID', count: 812, badge: false }]
			})
		);
		expect(body).not.toContain('PHPSESSID');
		// And the row says which absence it is rather than falling silent: it was
		// rendered and it does set cookies, so "no cookie" would be false.
		expect(body).toContain('cookies, none worth a badge');
	});

	// The finding this product exists for, and the one badge on a row that is not
	// a pivot: it does not link assets, it says one of them points at something
	// anybody can claim.
	it('renders a takeover candidate on the row, and links it to the search', () => {
		const body = row(
			asset({
				attributes: {
					takeover_candidate: { kind: 'cname', target: 'gone.s3.amazonaws.com' }
				}
			})
		);
		expect(body).toContain('takeover');
		expect(body).toContain('cname');
		expect(body).toContain('gone.s3.amazonaws.com');
		expect(body).toContain('href="/?f=takeover_candidate%3Aexists%3Atrue"');
	});

	// The fingerprinter runs on five triggers rather than on a cadence, so its
	// values can be weeks older than the last probe. A badge that shared the row's
	// timestamp would be claiming the browser saw what the probe saw.
	it('dates a fingerprinter badge separately from the row', () => {
		const body = row(
			asset({
				last_checked_at: '2026-08-18T11:00:00Z',
				last_fingerprint_at: '2026-07-01T09:00:00Z',
				pivots: [
					{ type: 'cookie_name', value: 'SESS', count: 4, badge: true },
					{ type: 'cert_spki', value: 'deadbeef', count: 2, badge: true }
				]
			})
		);
		// The cookie carries the render date, and says it is not the probe's.
		expect(body).toMatch(/SESS[\s\S]{0,400}?not when the row was last probed/);
		// The certificate does not: it comes from the probe, like the status code.
		expect(body).not.toMatch(/certificate key\.\s*Rendered/);
	});

	it('draws no script hash', () => {
		const body = row(asset({ pivots: [{ type: 'script', value: 'bundle', count: 12, badge: true }] }));
		expect(body).not.toContain('bundle');
	});

	it('shows the hops that led to the code, and no arrow on a single-hop chain', () => {
		expect(row(asset({ status_code: 200, status_chain: [301, 200] }))).toContain('301');
		const single = row(asset({ status_code: 200, status_chain: [200] }));
		expect(single.match(/>200</g)?.length).toBe(1);
	});

	// No composite score and no severity: the count says what it counts.
	it('carries the volatility as a number with its window in words', () => {
		expect(row(asset({ volatility: 4 }))).toContain('4 changes in 7 days');
	});
});

function group(assets: Asset[]): Group {
	return { host: 'a.acme.test', last_seen: '2026-08-18T11:00:00Z', assets };
}

const host = (g: Group, enriched = true) =>
	render(HostGroup, { props: { group: g, filters: [], enriched, favicons: {} } }).body;

describe('HostGroup', () => {
	it('states in the header what the services agree on', () => {
		const body = host(
			group([asset({ asn: 51167, asn_org: 'Contabo GmbH' }), asset({ asn: 51167, asn_org: 'Contabo GmbH' })])
		);
		expect(body).toContain('AS51167');
		expect(body).toContain('2 services');
	});

	// The discriminating case: two addresses under one name. A header that stated one
	// member's operator for both would be wrong in a way nothing on screen could show.
	it('states nothing the services disagree on', () => {
		const body = host(
			group([asset({ asn: 51167, asn_org: 'Contabo GmbH' }), asset({ asn: 62000, asn_org: 'Serverd SAS' })])
		);
		expect(body).not.toContain('AS51167');
		expect(body).not.toContain('AS62000');
	});

	// the three absences, first state: a deployment with no MaxMind database is a normal deployment,
	// so the family is not shown rather than shown empty.
	// The positive control, and without it the two cases below pass just as
	// happily on a console that shows nothing at all. That is how an absence
	// test goes quietly green: it asserts that something is missing, on a screen
	// where everything is.
	it('shows the operator and the place when the deployment enriches', () => {
		const body = host(group([asset({ asn: 51167, asn_org: 'Contabo GmbH', country: 'FR', city: 'Lauterbourg' })]));
		expect(body).toContain('AS51167');
		expect(body).toContain('Contabo GmbH');
		expect(body).toContain('Lauterbourg');
		// The flag is built from the two regional indicator letters rather than
		// shipped as an image, so it needs no request and no asset.
		expect(body).toContain('🇫🇷');
	});

	it('shows no operator at all when the deployment does not enrich', () => {
		const body = host(group([asset({ asn: 51167, asn_org: 'Contabo GmbH' })]), false);
		expect(body).not.toContain('AS51167');
	});

	// On a fronted asset the address is a point of presence, so the city would name
	// where the CDN is rather than where the asset is.
	it('drops the geolocation of a fronted host and keeps the CDN', () => {
		const fronted = asset({ is_cdn: true, cdn_provider: 'Cloudflare', country: 'FR', city: 'Paris' });
		const body = host(group([fronted, { ...fronted, asset_id: '2' }]));
		expect(body).toContain('CDN Cloudflare');
		expect(body).not.toContain('Paris');
	});

	// A group of one is a row, not a header over a single line: seventeen of the
	// thirty-three groups of a real inventory are a name nothing has probed.
	it('renders a host with no service as one row carrying its name', () => {
		const name = asset({
			kind: 'fqdn',
			key: 'a.acme.test',
			port: undefined,
			scheme: undefined,
			dns_state: 'unmeasured'
		});
		const body = host(group([name]));
		expect(body).toContain('0 services');
		expect(body).toContain('a.acme.test');
	});
});

describe('AssetRow landing', () => {
	// A service reached on :443 answers a final URL that spells the port the canonical
	// form drops, so a string comparison shows an arrow pointing at the address the row
	// already carries. What is shown is the difference, never the repetition.
	it('says nothing when the chain landed on the asset itself', () => {
		const body = row(asset({ status_code: 200, final_url: 'https://a.acme.test:443' }));
		expect(body).not.toContain('→');
	});

	it('shows the path alone when only the path changed', () => {
		const body = row(asset({ status_code: 200, final_url: 'https://a.acme.test/login?returnUrl=%2F' }));
		expect(body).toContain('/login?returnUrl=%2F');
		expect(body).not.toContain('→ https://a.acme.test/login');
	});

	// A service that sends somewhere else is a different fact, and the host is the half
	// of it that matters.
	it('shows the whole URL when the host changed', () => {
		const body = row(asset({ status_code: 302, final_url: 'https://elsewhere.test/' }));
		expect(body).toContain('https://elsewhere.test/');
	});
});

describe('the hops and the flat row', () => {
	// A hop is a status code and reads as one. Bare text made a 308 look like a
	// different kind of thing from the 200 it led to.
	it('gives a hop the colour family of its own code', () => {
		const body = row(asset({ status_code: 200, status_chain: [308, 302, 200] }));
		expect(body).toContain('data-code="3xx"');
		expect(body).toContain('data-code="2xx"');
	});

	// Flat, the address identifies the line, so it is not the 78px cell of a grouped
	// row where the header three lines up already carries the host.
	it('lays a flat row out differently from a grouped one', () => {
		expect(row(asset(), { withHost: true })).toContain('flat');
		expect(row(asset())).not.toContain('row flat');
	});
});

describe('Timeline diff rendering', () => {
	function change(field: Partial<import('$lib/types').DiffField>): import('$lib/types').Change {
		return {
			layer: 'tcp',
			at: '2026-08-18T09:00:00Z',
			held_until: '2026-08-18T11:00:00Z',
			outcome: 'ok',
			diff: {
				// The class sits on the transition and not on each field, because that
				// is what it describes: one field of a diff cannot be a revelation
				// while another is not.
				class: 'real_change',
				fields: [{ field: 'open_ports', kind: 'added', ...field }]
			}
		};
	}

	const timeline = (c: import('$lib/types').Change) =>
		render(Timeline, { props: { timeline: [c], truncated: [] } }).body;

	// internal/diff sends the whole field and the delta on purpose, and choosing
	// between them belongs to the screen. Rendering both wrote `→ 443 + 443`.
	it('writes a field that appeared as a delta and never as an arrow from nothing', () => {
		const body = timeline(change({ after: ['443'], added: ['443'] }));
		expect(body).toContain('+ 443');
		expect(body).not.toContain('→');
	});

	it('writes a short replacement as the pair it is', () => {
		const body = timeline(change({ field: 'status_code', before: ['200'], after: ['301'] }));
		expect(body).toContain('200');
		expect(body).toContain('301');
		expect(body).toContain('→');
	});

	// The pair is unreadable when there are ninety, which is the case the delta exists
	// for.
	it('writes a long list as the delta', () => {
		const many = Array.from({ length: 9 }, (_, i) => 'cookie_' + i);
		const body = timeline(change({ field: 'cookies', before: many, after: [...many, 'new'], added: ['new'] }));
		expect(body).toContain('+ new');
		expect(body).not.toContain('→');
	});
});
