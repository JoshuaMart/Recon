/**
 * Reading an observation payload without pretending it has a schema.
 *
 * The asset view reads the last observation of each layer twice: once curated, into the
 * panels somebody actually looks at, and once whole, into the raw fold. This module
 * is the first half, and it is the only place in the console that knows a path into
 * a payload.
 *
 * Every accessor is total: an absent key, a key of the wrong type and a producer that
 * renamed something all give `undefined` rather than a thrown page. That matters more
 * here than in most parsing code — the payload is written by a probe against a hostile
 * target, and the console renders it. The curated half being wrong must never cost the
 * raw half, which is what the fold shows and which needs no schema at all.
 *
 * The paths are those of the projection and internal/probe, and they are named here once so
 * that a rename in Go breaks one file rather than six components.
 */

import type { Evidence } from './types';

type Bag = Record<string, unknown>;

function bag(value: unknown): Bag {
	return value !== null && typeof value === 'object' && !Array.isArray(value) ? (value as Bag) : {};
}

function text(value: unknown): string | undefined {
	return typeof value === 'string' && value !== '' ? value : undefined;
}

function count(value: unknown): number | undefined {
	return typeof value === 'number' && Number.isFinite(value) ? value : undefined;
}

function list(value: unknown): unknown[] {
	return Array.isArray(value) ? value : [];
}

/** The last observation of one layer, or nothing when it has never been observed. */
export function layerOf(evidence: Evidence[], layer: string): Evidence | undefined {
	return evidence.find((entry) => entry.layer === layer);
}

/**
 * Headers, lowercased.
 *
 * HTTP header names are case insensitive and a producer echoes whatever the target
 * sent, so a lookup on the spelling of the day is a lookup that misses. A repeated
 * header arrives as an array and is joined rather than dropped.
 */
export function headersOf(value: unknown): Record<string, string> {
	const out: Record<string, string> = {};
	for (const [name, raw] of Object.entries(bag(value))) {
		const joined = Array.isArray(raw)
			? raw.filter((item): item is string => typeof item === 'string').join(', ')
			: typeof raw === 'string'
				? raw
				: undefined;
		if (joined) out[name.toLowerCase()] = joined;
	}
	return out;
}

/** One step of a redirect chain. */
export interface Hop {
	code?: number;
	url?: string;
	/** Where this hop said to go next, which is the only header worth the line. */
	location?: string;
	contentType?: string;
	size?: number;
}

/**
 * The chain, from the payload.
 *
 * `http-check` writes `code` on a hop and the fingerprinter writes `status_code`, so
 * both are read. That divergence is real and this is the cheapest place to absorb it:
 * the alternative is a component that renders one producer and blanks on the other.
 */
export function hopsOf(evidence: Evidence | undefined): Hop[] {
	return list(bag(evidence?.data).chain).map((raw) => {
		const hop = bag(raw);
		const headers = headersOf(hop.headers);
		return {
			code: count(hop.code) ?? count(hop.status_code),
			url: text(hop.url),
			location: headers['location'],
			contentType: headers['content-type'],
			size: count(hop.response_size) ?? count(hop.size)
		};
	});
}

/** The headers of the response that answered, which is the last hop's. */
export function responseHeaders(evidence: Evidence | undefined): Record<string, string> {
	return headersOf(bag(evidence?.data).headers);
}

export interface Certificate {
	version?: string;
	cipher?: string;
	issuer?: string;
	subject?: string;
	san: string[];
	notBefore?: string;
	notAfter?: string;
	spki?: string;
}

/**
 * The certificate, or nothing when no handshake ever completed.
 *
 * Nothing rather than an empty object: "no TLS" and "TLS whose fields are all empty"
 * are different sentences, and only the first one is ever true here.
 */
export function certificateOf(evidence: Evidence | undefined): Certificate | undefined {
	const tls = bag(bag(evidence?.data).tls);
	if (Object.keys(tls).length === 0) return undefined;

	const san = text(tls.san) ? [tls.san as string] : list(tls.san).filter((n): n is string => typeof n === 'string');
	return {
		version: text(tls.version),
		cipher: text(tls.cipher),
		issuer: text(tls.issuer),
		subject: text(tls.subject),
		san,
		notBefore: text(tls.not_before),
		notAfter: text(tls.not_after),
		spki: text(tls.cert_spki_hash)
	};
}

/**
 * The security headers the panel reports on, present or absent.
 *
 * This is an allowlist, which the pivot rules refuse and which is right here for the
 * opposite reason: the value of the block is that it names what is **missing**, and a
 * denylist cannot name an absence. What it costs is that a header nobody listed does
 * not appear in this block — it appears in the raw fold, which shows every key and is
 * where the denylist rule keeps applying.
 *
 * No score, no letter, no colour that ranks them. A header is a fact; a grade would be
 * the composite severity the jalon 7 forbids on an asset.
 */
const securityNames: { name: string; label: string }[] = [
	{ name: 'strict-transport-security', label: 'Strict-Transport-Security' },
	{ name: 'content-security-policy', label: 'Content-Security-Policy' },
	{ name: 'x-frame-options', label: 'X-Frame-Options' },
	{ name: 'x-content-type-options', label: 'X-Content-Type-Options' },
	{ name: 'referrer-policy', label: 'Referrer-Policy' },
	{ name: 'permissions-policy', label: 'Permissions-Policy' },
	{ name: 'cross-origin-opener-policy', label: 'Cross-Origin-Opener-Policy' },
	{ name: 'cross-origin-resource-policy', label: 'Cross-Origin-Resource-Policy' }
];

export interface SecurityHeader {
	name: string;
	label: string;
	value?: string;
}

export function securityHeaders(headers: Record<string, string>): SecurityHeader[] {
	return securityNames.map((entry) => ({ ...entry, value: headers[entry.name] }));
}

/** One script the render loaded. */
export interface Script {
	url?: string;
	hash?: string;
	internal: boolean;
}

/**
 * The internal scripts of a render, which is where their hashes belong.
 *
 * the three absences took them off the card at 464 badges for 50 rows, and said in the same breath
 * that they stay a pivot in the search and in this view. External ones are dropped for
 * the reason the pivot rules give: a bundle served from a public CDN groups without
 * discriminating.
 */
export function scriptsOf(evidence: Evidence | undefined): Script[] {
	return list(bag(evidence?.data).scripts)
		.map((raw) => {
			const script = bag(raw);
			return { url: text(script.url), hash: text(script.hash), internal: script.internal === true };
		})
		.filter((script) => script.internal && (script.hash || script.url));
}

export interface RenderFacts {
	faviconHash?: string;
	faviconURL?: string;
	robots?: boolean;
	llms?: boolean;
	cname?: string;
	cookieNames: string[];
	externalHosts: string[];
}

/** What the render says about the page itself, outside the pivots the card carries. */
export function renderFacts(evidence: Evidence | undefined): RenderFacts {
	const data = bag(evidence?.data);
	const metadata = bag(data.metadata);
	const network = bag(data.network);
	return {
		faviconHash: text(metadata.favicon_hash),
		faviconURL: text(metadata.favicon_url),
		robots: typeof metadata.robots_txt === 'boolean' ? metadata.robots_txt : undefined,
		llms: typeof metadata.llms_txt === 'boolean' ? metadata.llms_txt : undefined,
		cname: text(network.cname),
		cookieNames: Object.keys(bag(data.cookies)),
		externalHosts: list(data.external_hosts).filter((host): host is string => typeof host === 'string')
	};
}

export interface Ports {
	open: number[];
	/** How many ports the scan actually tried, which is what says it ran at all. */
	scanned?: number;
}

/**
 * The ports, counted rather than listed.
 *
 * `evidence.ts` hides `scanned_ports`, `closed_ports` and `filtered_ports` from the raw
 * tree because a hundred numbers identical on every asset are the probe's settings
 * echoed once per row. The **count** is a different thing: it separates "nothing else
 * is open" from "nothing else was tried", and those two are not the same finding.
 */
export function portsOf(evidence: Evidence | undefined): Ports {
	const data = bag(evidence?.data);
	const numbers = (value: unknown) => list(value).filter((port): port is number => typeof port === 'number');
	const scanned = numbers(data.scanned_ports).length;
	return { open: numbers(data.open_ports), scanned: scanned > 0 ? scanned : undefined };
}

/** The cookie names `http-check` saw in `Set-Cookie`, which is not the same producer as the render. */
export function cookieNamesOf(evidence: Evidence | undefined): string[] {
	return list(bag(evidence?.data).cookie_names).filter((name): name is string => typeof name === 'string');
}

/** Whatever the probe recorded about why nothing was obtained. */
export function failureOf(evidence: Evidence | undefined): string | undefined {
	const data = bag(evidence?.data);
	return text(data.error) ?? text(data.reason) ?? text(data.origin_error);
}
