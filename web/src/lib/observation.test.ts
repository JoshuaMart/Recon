import { describe, expect, it } from 'vitest';
import {
	certificateOf,
	cookieNamesOf,
	headersOf,
	hopsOf,
	layerOf,
	portsOf,
	renderFacts,
	scriptsOf,
	securityHeaders
} from './observation';
import type { Evidence } from './types';

/** An observation, with only the fields these accessors read. */
function observation(layer: Evidence['layer'], data: unknown): Evidence {
	return {
		layer,
		outcome: 'ok',
		source: layer + '-check',
		observed_at: '2026-08-18T09:00:00Z',
		last_confirmed_at: '2026-08-18T11:00:00Z',
		data: data as Record<string, unknown>
	};
}

describe('hopsOf', () => {
	// The case that decides the module: `http-check` writes `code` on a hop and the
	// fingerprinter writes `status_code` for the same thing. A reader of one spelling
	// renders one producer and blanks on the other, which is a blank nobody explains.
	it('reads both spellings of a hop status', () => {
		const probe = hopsOf(observation('http', { chain: [{ code: 301, url: 'http://a.example' }] }));
		const render = hopsOf(observation('fingerprint', { chain: [{ status_code: 200, url: 'https://a.example' }] }));
		expect(probe[0].code).toBe(301);
		expect(render[0].code).toBe(200);
	});

	it('lifts the location of a hop out of its headers', () => {
		const hops = hopsOf(
			observation('http', {
				chain: [{ code: 301, url: 'http://a.example', headers: { Location: 'https://a.example/' } }]
			})
		);
		expect(hops[0].location).toBe('https://a.example/');
	});

	it('answers an empty chain rather than throwing on a payload without one', () => {
		expect(hopsOf(observation('http', { status_code: 200 }))).toEqual([]);
		expect(hopsOf(undefined)).toEqual([]);
		expect(hopsOf(observation('http', { chain: 'not a chain' }))).toEqual([]);
	});
});

describe('headersOf', () => {
	// Header names are case insensitive and a producer echoes what the target sent, so a
	// lookup on the spelling of the day is a lookup that misses.
	it('lowercases the names', () => {
		expect(headersOf({ 'Content-Type': 'text/html' })['content-type']).toBe('text/html');
	});

	it('joins a repeated header rather than dropping it', () => {
		expect(headersOf({ 'set-cookie': ['a=1', 'b=2'] })['set-cookie']).toBe('a=1, b=2');
	});
});

describe('securityHeaders', () => {
	// The whole value of the block is that it names what is **missing**, which is why it
	// is the one allowlist in the console: a denylist cannot name an absence.
	it('names an absent header instead of omitting it', () => {
		const out = securityHeaders({ 'strict-transport-security': 'max-age=31536000' });
		const csp = out.find((header) => header.name === 'content-security-policy');
		expect(csp).toBeDefined();
		expect(csp?.value).toBeUndefined();
		expect(out.find((header) => header.name === 'strict-transport-security')?.value).toBe('max-age=31536000');
	});
});

describe('certificateOf', () => {
	it('is nothing when no handshake completed', () => {
		expect(certificateOf(observation('http', { status_code: 200 }))).toBeUndefined();
	});

	it('reads a san given as one name or as a list', () => {
		expect(certificateOf(observation('http', { tls: { san: 'a.example' } }))?.san).toEqual(['a.example']);
		expect(certificateOf(observation('http', { tls: { san: ['a.example', 'b.example'] } }))?.san).toEqual([
			'a.example',
			'b.example'
		]);
	});

	it('carries the key hash, which is the pivot that survives a renewal', () => {
		const certificate = certificateOf(observation('http', { tls: { cert_spki_hash: '561d31de', version: 'TLS 1.3' } }));
		expect(certificate?.spki).toBe('561d31de');
		expect(certificate?.version).toBe('TLS 1.3');
	});
});

describe('scriptsOf', () => {
	// Internal scripts only: a bundle served from a public CDN is shared by
	// thousands of sites that have nothing to do with each other, so it groups without
	// discriminating, which is the test JARM failed.
	it('drops the external ones', () => {
		const out = scriptsOf(
			observation('fingerprint', {
				scripts: [
					{ url: 'https://a.example/app.js', hash: 'aaaa', internal: true },
					{ url: 'https://cdn.example/jquery.js', hash: 'bbbb', internal: false }
				]
			})
		);
		expect(out).toHaveLength(1);
		expect(out[0].hash).toBe('aaaa');
	});
});

describe('renderFacts', () => {
	// `false` is a measurement and `undefined` is the absence of one. Rendering the two
	// the same way is the mistake the three absences spends its length avoiding, one level down.
	it('separates a robots.txt that is absent from one that was never looked for', () => {
		expect(renderFacts(observation('fingerprint', { metadata: { robots_txt: false } })).robots).toBe(false);
		expect(renderFacts(observation('fingerprint', { metadata: {} })).robots).toBeUndefined();
	});

	it('takes the cookie names from the keys, the values having been redacted', () => {
		const facts = renderFacts(observation('fingerprint', { cookies: { _lighthouse_session: 'redacted' } }));
		expect(facts.cookieNames).toEqual(['_lighthouse_session']);
	});
});

describe('portsOf', () => {
	// The count and never the list: `evidence.ts` hides `scanned_ports` because a hundred
	// numbers identical on every asset are the probe's settings echoed once per row. The
	// count is what separates "nothing else is open" from "nothing else was tried".
	it('counts what was scanned without carrying it', () => {
		const ports = portsOf(observation('tcp', { open_ports: [80], scanned_ports: [80, 443, 8080] }));
		expect(ports.open).toEqual([80]);
		expect(ports.scanned).toBe(3);
	});

	it('has no count when the payload carries no scan', () => {
		expect(portsOf(observation('tcp', { open_ports: [80] })).scanned).toBeUndefined();
	});
});

describe('layerOf and cookieNamesOf', () => {
	it('finds a layer and answers nothing for one that was never observed', () => {
		const evidence = [observation('http', {}), observation('tcp', {})];
		expect(layerOf(evidence, 'http')?.layer).toBe('http');
		expect(layerOf(evidence, 'fingerprint')).toBeUndefined();
	});

	it('reads the cookie names http-check saw in Set-Cookie', () => {
		expect(cookieNamesOf(observation('http', { cookie_names: ['a', 'b'] }))).toEqual(['a', 'b']);
		expect(cookieNamesOf(observation('http', {}))).toEqual([]);
	});
});
