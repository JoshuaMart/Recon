import { describe, expect, it } from 'vitest';
import { frontDoorFavicon, rowBadges, sharedOf, splitGroup } from './group';
import type { Asset, Group, Pivot } from './types';

function asset(overrides: Partial<Asset> = {}): Asset {
	return {
		asset_id: crypto.randomUUID(),
		program_id: '00000000-0000-0000-0000-0000000000ff',
		discovery_source: 'fastrecon',
		kind: 'service',
		key: 'a.acme.test:443/tcp',
		host: 'a.acme.test',
		scope_status: 'in_scope',
		lifecycle: 'active',
		technologies: [],
		attributes: {},
		volatility: 0,
		first_seen: '2026-08-18T09:00:00Z',
		last_seen: '2026-08-18T11:00:00Z',
		lineage: [{ step: 'derived', host: 'a.acme.test', port: 443 }],
		...overrides
	};
}

function group(assets: Asset[]): Group {
	return { host: 'a.acme.test', last_seen: '2026-08-18T11:00:00Z', assets };
}

const cert = (value: string, count = 2): Pivot => ({ type: 'cert_spki', value, count, badge: true });

describe('sharedOf', () => {
	it('raises what every service agrees on', () => {
		const shared = sharedOf(
			group([
				asset({ asn: 51167, asn_org: 'Contabo GmbH', country: 'FR', city: 'Lauterbourg', ip: '1.2.3.4' }),
				asset({ asn: 51167, asn_org: 'Contabo GmbH', country: 'FR', city: 'Lauterbourg', ip: '1.2.3.4' })
			])
		);
		expect(shared.asn).toBe(51167);
		expect(shared.city).toBe('Lauterbourg');
		expect(shared.ip).toBe('1.2.3.4');
	});

	// The case the module exists for. A host with two addresses would otherwise have one
	// member's operator stated as the host's, and nothing on screen would say which
	// member it came from.
	it('raises nothing the services disagree on', () => {
		const shared = sharedOf(group([asset({ asn: 51167, ip: '1.2.3.4' }), asset({ asn: 62000, ip: '5.6.7.8' })]));
		expect(shared.asn).toBeUndefined();
		expect(shared.ip).toBeUndefined();
	});

	// A member that carries no value does not vote: an http-only service has no
	// certificate, and letting its absence veto would hide a certificate that every
	// service presenting one agrees on.
	it('lets a service with no certificate abstain rather than veto', () => {
		const shared = sharedOf(
			group([asset({ pivots: [cert('561d31de')] }), asset({ key: 'a.acme.test:80/tcp', pivots: [] })])
		);
		expect(shared.cert?.value).toBe('561d31de');
		expect(shared.cert?.count).toBe(2);
	});

	it('keeps a certificate on the rows when two services present different ones', () => {
		const shared = sharedOf(group([asset({ pivots: [cert('561d31de')] }), asset({ pivots: [cert('29989f8f')] })]));
		expect(shared.cert).toBeUndefined();
	});

	// Lineage is per asset , and on the real inventory two hosts came from two
	// different modules. Folding it to the host means picking one member's path, so it
	// only folds when there is nothing to pick between.
	it('folds the lineage only when every service came the same way', () => {
		const same = sharedOf(group([asset(), asset()]));
		expect(same.lineage).toBe('derived');

		const differing = sharedOf(group([asset(), asset({ lineage: [{ step: 'enumerated' }] })]));
		expect(differing.lineage).toBeUndefined();
	});

	it('reads the favicon the whole host shows', () => {
		const shared = sharedOf(
			group([asset({ attributes: { favicon_hash: '47625750' } }), asset({ attributes: { favicon_hash: '47625750' } })])
		);
		expect(shared.favicon).toBe('47625750');
	});
});

describe('rowBadges', () => {
	it('drops what the header already states and keeps what differs', () => {
		const shared = sharedOf(
			group([
				asset({
					attributes: { favicon_hash: 'shared' },
					pivots: [{ type: 'favicon', value: 'shared', count: 5, badge: true }]
				}),
				asset({
					attributes: { favicon_hash: 'shared' },
					pivots: [{ type: 'favicon', value: 'shared', count: 5, badge: true }]
				})
			])
		);
		const same = asset({ pivots: [{ type: 'favicon', value: 'shared', count: 5, badge: true }] });
		const different = asset({ pivots: [{ type: 'favicon', value: '其他', count: 12, badge: true }] });

		expect(rowBadges(same, shared)).toHaveLength(0);
		expect(rowBadges(different, shared)).toHaveLength(1);
	});

	// Cookie names are per service by nature, and their absence carries the three states
	// milestone 7 asserts, so they never fold into the header.
	it('always keeps the cookie names', () => {
		const shared = sharedOf(group([asset()]));
		const row = asset({ pivots: [{ type: 'cookie_name', value: 'sess', count: 3, badge: true }] });
		expect(rowBadges(row, shared).map((pivot: Pivot) => pivot.value)).toEqual(['sess']);
	});

	// The list stops receiving them server-side, and a console pointed at an older
	// control plane must not start drawing them again.
	it('drops a script hash whatever the header says', () => {
		const shared = sharedOf(group([asset()]));
		const row = asset({ pivots: [{ type: 'script', value: 'bundle', count: 12, badge: true }] });
		expect(rowBadges(row, shared)).toHaveLength(0);
	});
});

describe('splitGroup', () => {
	it('folds the name of the host into the header and leaves the services as rows', () => {
		const name = asset({ kind: 'fqdn', key: 'a.acme.test' });
		const service = asset();
		const { self, services } = splitGroup(group([name, service]));
		expect(self).toBe(name);
		expect(services).toEqual([service]);
	});

	it('has no self on a host whose name is not an asset of ours', () => {
		const { self, services } = splitGroup(group([asset(), asset()]));
		expect(self).toBeUndefined();
		expect(services).toHaveLength(2);
	});
});

describe('frontDoorFavicon', () => {
	// A host serving four applications agreed on no favicon and got an empty square,
	// which reads as a missing image rather than as four different ones. The header may
	// not *claim* a favicon for the host, and it may still *show* one.
	it('takes the https service on 443 when the services differ', () => {
		const hash = frontDoorFavicon(
			group([
				asset({ port: 8081, scheme: 'http', attributes: { favicon_hash: 'dockarr' } }),
				asset({ port: 443, scheme: 'https', attributes: { favicon_hash: 'front' } }),
				asset({ port: 5000, scheme: 'http', attributes: { favicon_hash: 'kavita' } })
			])
		);
		expect(hash).toBe('front');
	});

	it('falls back to :80, then to whatever answered', () => {
		expect(
			frontDoorFavicon(
				group([
					asset({ port: 8080, scheme: 'http', attributes: { favicon_hash: 'other' } }),
					asset({ port: 80, scheme: 'http', attributes: { favicon_hash: 'plain' } })
				])
			)
		).toBe('plain');
		expect(frontDoorFavicon(group([asset({ port: 9000, attributes: { favicon_hash: 'only' } })]))).toBe('only');
	});

	it('has nothing to show on a host no render ever reached', () => {
		expect(frontDoorFavicon(group([asset(), asset()]))).toBeUndefined();
	});
});
