import { describe, expect, it } from 'vitest';
import {
	ago,
	bytes,
	cardHashes,
	cookieState,
	flag,
	geoVisible,
	identity,
	infraState,
	daysUntil,
	elapsed,
	landingOf,
	lineage,
	verdictOf,
	shownFacets,
	webSurface
} from './format';
import type { Asset, Facet } from './types';

/**
 * The cases here are the ones that discriminate, which is the only kind worth
 * writing: roadmap rule 8 comes from two defects that were invisible because the
 * test data had no wildcard and never crossed midnight.
 */

function base(): Asset {
	return asset();
}

function asset(overrides: Partial<Asset> = {}): Asset {
	return {
		asset_id: '00000000-0000-0000-0000-000000000001',
		program_id: '00000000-0000-0000-0000-0000000000ff',
		discovery_source: 'manual',
		kind: 'url',
		key: 'https://a.example.com/',
		scope_status: 'in_scope',
		lifecycle: 'active',
		technologies: [],
		attributes: {},
		volatility: 0,
		first_seen: '2026-01-01T00:00:00Z',
		last_seen: '2026-01-01T00:00:00Z',
		...overrides
	};
}

describe('cookieState', () => {
	it('separates never rendered from rendered with no cookie', () => {
		expect(cookieState(asset())).toBe('never-rendered');
		expect(cookieState(asset({ last_fingerprint_at: '2026-08-01T00:00:00Z' }))).toBe('none');
	});

	it('does not report a site that only sets PHPSESSID as setting no cookie', () => {
		// The third case of the three absences, and the reason this function exists rather
		// than a check on the badge list. The asset was rendered and does set a
		// cookie; both display filters removed it. Calling that "no cookie" would
		// state something false, which is what the denylist is forbidden to do.
		const filtered = asset({
			last_fingerprint_at: '2026-08-01T00:00:00Z',
			attributes: { cookie_names: ['PHPSESSID'] },
			pivots: []
		});
		expect(cookieState(filtered)).toBe('all-filtered');
	});

	it('reports a badge when one survived', () => {
		const shown = asset({
			last_fingerprint_at: '2026-08-01T00:00:00Z',
			attributes: { cookie_names: ['SESS_INTERNAL'] },
			pivots: [{ type: 'cookie_name', value: 'SESS_INTERNAL', count: 4, badge: true }]
		});
		expect(cookieState(shown)).toBe('shown');
	});
});

describe('geoVisible', () => {
	it('hides the geolocation of a fronted asset and keeps it otherwise', () => {
		// On a fronted target the address is that of a point of presence, so
		// the city says where the CDN is, not where the asset is.
		expect(geoVisible(asset({ is_cdn: true, country: 'US' }))).toBe(false);
		expect(geoVisible(asset({ is_cdn: false, country: 'FR' }))).toBe(true);
	});

	it('hides it when there is no country to show', () => {
		expect(geoVisible(asset({ is_cdn: false }))).toBe(false);
	});
});

describe('identity', () => {
	it('splits a url into its origin and its path', () => {
		expect(identity(asset({ key: 'https://a.example.com/login' }))).toEqual({
			head: 'https://a.example.com',
			path: '/login'
		});
	});

	it('writes a service as the URL somebody would open', () => {
		expect(
			identity(
				asset({
					kind: 'service',
					key: 'a.example.com:443/tcp',
					host: 'a.example.com',
					port: 443,
					scheme: 'https',
					http_state: 'healthy'
				})
			)
		).toEqual({ head: 'https://a.example.com', path: '' });
	});

	// The case the port table got wrong: 8081 answered in plain http, the probe
	// recorded it, and the card refused to say so because the number is ambiguous.
	// The number is; the observation is not.
	it('writes the scheme the probe measured on an unusual port', () => {
		expect(
			identity(
				asset({
					kind: 'service',
					key: 'a.example.com:8081/tcp',
					host: 'a.example.com',
					port: 8081,
					scheme: 'http',
					http_state: 'healthy'
				})
			).head
		).toBe('http://a.example.com:8081');
	});

	// A TLS listener on 8080 is exactly why the port cannot be trusted, and exactly
	// what a measured scheme handles without a special case.
	it('believes the measurement over the number', () => {
		expect(
			identity(
				asset({
					kind: 'service',
					key: 'a.example.com:8080/tcp',
					host: 'a.example.com',
					port: 8080,
					scheme: 'https',
					http_state: 'healthy'
				})
			).head
		).toBe('https://a.example.com:8080');
	});

	// And a service nothing made answer is written as what it is. 443 open is not
	// 443 answering, and no scheme was established.
	it('writes host:port on a service with no measured scheme', () => {
		expect(identity(asset({ kind: 'service', key: 'a.example.com:443/tcp', port: 443 })).head).toBe(
			'a.example.com:443'
		);
	});

	it('keeps a protocol that is not tcp', () => {
		expect(identity(asset({ kind: 'service', key: 'a.example.com:161/udp', port: 161 })).head).toBe(
			'a.example.com:161/udp'
		);
	});

	it('does not split a url kind whose key will not parse', () => {
		expect(identity(asset({ kind: 'url', key: 'not a url' })).head).toBe('not a url');
	});
});

describe('ago', () => {
	const now = Date.parse('2026-08-17T12:00:00Z');

	it('reads in the unit somebody would use out loud', () => {
		expect(ago('2026-08-17T11:59:30Z', now)).toBe('just now');
		expect(ago('2026-08-17T11:20:00Z', now)).toBe('40 min ago');
		expect(ago('2026-08-17T06:00:00Z', now)).toBe('6 h ago');
		expect(ago('2026-08-10T12:00:00Z', now)).toBe('7 d ago');
		expect(ago('2026-05-17T12:00:00Z', now)).toBe('3 months ago');
		expect(ago('2025-05-17T12:00:00Z', now)).toBe('1 year ago');
	});

	it('stays on days up to six weeks', () => {
		// The boundary is the point of the test. Rounding 40 days to "1 month ago"
		// loses exactly the distinction the temporal band exists for.
		expect(ago('2026-07-08T12:00:00Z', now)).toBe('40 d ago');
	});

	it('says never rather than inventing a duration', () => {
		expect(ago(undefined, now)).toBe('never');
		expect(ago('not a date', now)).toBe('never');
	});
});

describe('lineage', () => {
	it('shows the step that produced the asset, which is the last one', () => {
		const path = [
			{ step: 'enumerated', sources: ['crtsh'] },
			{ step: 'derived', port: 8443 }
		];
		expect(lineage(asset({ lineage: path }))).toBe('derived');
	});

	it('falls back to the source, then says there is none', () => {
		expect(lineage(asset({ discovery_source: 'manual' }))).toBe('found by manual');
		// An entry with no step name is not a step. Rendering the object would put
		// a structure where a sentence belongs.
		expect(lineage(asset({ lineage: [{ run: 'abc' }], discovery_source: '' }))).toBe('no recorded lineage');
		expect(lineage(asset({ discovery_source: '' }))).toBe('no recorded lineage');
	});
});

describe('shownFacets', () => {
	it('survives a facet whose terms are null', () => {
		// The case that took the first render of a real inventory down. The server
		// returns one facet per field whether or not the filtered set has a value
		// for it, and a nil slice encodes as `null` in Go, not as [].
		const facets: Facet[] = [
			{ field: 'port', terms: null },
			{ field: 'lifecycle', terms: [{ value: 'active', count: 35 }] }
		];
		expect(shownFacets(facets)).toEqual([
			{ field: 'lifecycle', terms: [{ value: 'active', count: 35 }], truncated: false }
		]);
	});

	it('drops an empty array too, and keeps the order asked for', () => {
		const facets: Facet[] = [
			{ field: 'country', terms: [] },
			{ field: 'port', terms: [{ value: '443', count: 2 }] },
			{ field: 'kind', terms: [{ value: 'url', count: 1 }] }
		];
		expect(shownFacets(facets).map((facet) => facet.field)).toEqual(['port', 'kind']);
	});

	it('carries the truncation through, because a cut list must say so', () => {
		const facets: Facet[] = [{ field: 'port', terms: [{ value: '443', count: 2 }], truncated: true }];
		expect(shownFacets(facets)[0].truncated).toBe(true);
	});
});

describe('cardHashes', () => {
	it('keeps the favicon and the certificate key, and drops the scripts', () => {
		// the three absences, measured: 464 badges over 50 cards, 316 distinct script values.
		// The scripts leave the card and stay in the search, so the assertion is on
		// what the card shows and not on what the server sent.
		const asset = {
			...base(),
			pivots: [
				{ type: 'script' as const, value: 'aaa', count: 4, badge: true },
				{ type: 'favicon' as const, value: 'bbb', count: 7, badge: true },
				{ type: 'cert_spki' as const, value: 'ccc', count: 2, badge: true },
				{ type: 'cookie_name' as const, value: 'SESS', count: 3, badge: true }
			]
		};
		expect(cardHashes(asset).map((badge) => badge.type)).toEqual(['favicon', 'cert_spki']);
	});

	it('is empty rather than undefined when the server sent no badge', () => {
		expect(cardHashes(base())).toEqual([]);
	});
});

describe('infraState', () => {
	it('says unconfigured before looking at the asset', () => {
		// The case that matters. A deployment with no MaxMind database is normal
		// , and an empty infrastructure family reads as a broken interface.
		// It wins over the CDN case, since neither ASN nor country exists to hide.
		expect(infraState(base(), false)).toBe('unconfigured');
		expect(infraState({ ...base(), is_cdn: true, asn: 13335 }, false)).toBe('unconfigured');
	});

	it('separates a fronted asset from one the database does not cover', () => {
		expect(infraState({ ...base(), is_cdn: true, asn: 13335 }, true)).toBe('cdn');
		expect(infraState(base(), true)).toBe('no-match');
		expect(infraState({ ...base(), asn: 16276, country: 'FR' }, true)).toBe('shown');
	});
});

describe('flag', () => {
	it('builds a flag from a country code', () => {
		expect(flag('FR')).toBe('🇫🇷');
		expect(flag('us')).toBe('🇺🇸');
	});

	it('gives nothing rather than mojibake on anything else', () => {
		// The codes come from a MaxMind lookup, so a two-letter shape is expected —
		// but an empty string, a three-letter code or a digit must not render two
		// arbitrary code points that look like a flag of somewhere.
		for (const bad of [undefined, '', 'F', 'FRA', '12', 'F1']) {
			expect(flag(bad)).toBe('');
		}
	});
});

describe('landingOf', () => {
	// The ordinary case, and the reason the function exists: a service on 443 that
	// answers 200 three hops later, on a page whose path is the whole news.
	it('shows the path when the host did not change', () => {
		const a = asset({
			kind: 'service',
			key: 'seerr.example.com:443/tcp',
			port: 443,
			http_state: 'healthy',
			final_url: 'https://seerr.example.com/login'
		});
		expect(landingOf(a, 'https://seerr.example.com')).toBe('/login');
	});

	// The discriminating case: a redirect off the host is a different fact, and the
	// host is the half of it that matters, so the whole URL is shown.
	it('shows the whole url when the host changed', () => {
		const a = asset({ kind: 'service', key: 'a.example.com:80/tcp', final_url: 'https://b.other.test/' });
		expect(landingOf(a, 'http://a.example.com')).toBe('https://b.other.test/');
	});

	// Landing on itself is not news. A card that printed "→ /" on every unredirected
	// service would add a column of noise to the majority of the list.
	it('says nothing when the service landed on its own root', () => {
		const a = asset({ kind: 'service', key: 'a.example.com:443/tcp', final_url: 'https://a.example.com/' });
		expect(landingOf(a, 'https://a.example.com')).toBe('');
	});

	it('says nothing when nothing has been fetched', () => {
		expect(landingOf(asset({ kind: 'service', key: 'a.example.com:443/tcp' }), 'https://a.example.com')).toBe('');
	});

	// A malformed value is what the probe recorded, so it is shown rather than
	// swallowed: a broken Location header is a finding, not a rendering problem.
	it('shows a value that will not parse', () => {
		const a = asset({ kind: 'service', key: 'a.example.com:443/tcp', final_url: 'not a url' });
		expect(landingOf(a, 'https://a.example.com')).toBe('not a url');
	});
});

describe('webSurface', () => {
	// A name and an address are probed at dns and tcp, never at http. Everything
	// a card says about a response is, for them, a sentence about a question nobody
	// asked — and "no answer" to such a question is worse than silence.
	it('separates what is probed over http from what is not', () => {
		expect(webSurface(asset({ kind: 'service', key: 'a.example.com:443/tcp' }))).toBe(true);
		expect(webSurface(asset({ kind: 'url', key: 'https://a.example.com/api' }))).toBe(true);
		expect(webSurface(asset({ kind: 'fqdn', key: 'a.example.com' }))).toBe(false);
		expect(webSurface(asset({ kind: 'ip', key: '1.2.3.4' }))).toBe(false);
	});
});

describe('bytes', () => {
	// The exact number, grouped: it is what somebody compares against the next
	// observation of the same asset, and a rounded one hides a body that grew by eighty
	// bytes.
	it('groups rather than abbreviates under a megabyte', () => {
		expect(bytes(4637)).toBe('4 637 B');
		expect(bytes(0)).toBe('0 B');
		expect(bytes(2_400_000)).toBe('2.4 MB');
	});

	it('says nothing when the producer sent no size', () => {
		expect(bytes(undefined)).toBe('');
	});
});

describe('daysUntil and elapsed', () => {
	const now = Date.parse('2026-08-18T12:00:00Z');

	it('counts whole days, and goes negative once the instant has passed', () => {
		expect(daysUntil('2026-11-15T10:30:57Z', now)).toBe(88);
		expect(daysUntil('2026-08-10T12:00:00Z', now)).toBe(-8);
		expect(daysUntil(undefined, now)).toBeUndefined();
	});

	// Clamped at both ends: an expired certificate draws a full bar rather than one
	// longer than its track, and one issued in the future draws an empty one.
	it('clamps the progress of a window at both ends', () => {
		expect(elapsed('2026-08-17T00:00:00Z', '2026-11-15T00:00:00Z', now)).toBe(2);
		expect(elapsed('2026-01-01T00:00:00Z', '2026-02-01T00:00:00Z', now)).toBe(100);
		expect(elapsed('2026-12-01T00:00:00Z', '2027-01-01T00:00:00Z', now)).toBe(0);
		expect(elapsed(undefined, '2026-11-15T00:00:00Z', now)).toBe(0);
	});
});

describe('verdictOf', () => {
	// the three absences: "no observer gets through" is an absence of measurement and "the name no
	// longer resolves" is a measurement. The first licenses no conclusion, so the two
	// carry different tones as well as different words.
	it('separates an absence of measurement from a measurement', () => {
		const unobservable = verdictOf(asset({ lifecycle: 'unobservable' }));
		const inactive = verdictOf(asset({ lifecycle: 'inactive', dns_state: 'dead' }));
		expect(unobservable?.tone).toBe('unobs');
		expect(unobservable?.text).toContain('not called dead');
		expect(inactive?.tone).toBe('dead');
		expect(inactive?.text).toContain('no longer resolves');
	});

	it('says why an asset outside the decided perimeter is never probed', () => {
		expect(verdictOf(asset({ scope_status: 'unknown' }))?.text).toContain('never probed');
	});

	it('has nothing to say about an ordinary active asset', () => {
		expect(verdictOf(asset())).toBeNull();
	});
});
