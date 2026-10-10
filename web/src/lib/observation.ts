/**
 * Reading an observation payload without pretending it has a schema. The asset view reads each
 * layer's last observation twice, curated into panels and whole into the raw fold; this is the first
 * half and the only place that knows a path into a payload, so a Go rename breaks one file.
 *
 * Every accessor is total: an absent key, wrong type or renamed field gives `undefined`, never a
 * thrown page. The payload comes from a probe against a hostile target, and a wrong curated half
 * must never cost the raw half, which needs no schema.
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
 * Headers, lowercased. Names are case insensitive and a producer echoes what the target sent,
 * so a lookup on the spelling of the day misses. A repeated header arrives as an array and is
 * joined, not dropped.
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
 * The chain, from the payload. `http-check` writes `code` on a hop and the fingerprinter
 * `status_code`, so both are read: a real divergence, cheapest to absorb here instead of a
 * component that renders one producer and blanks on the other.
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
 * The certificate, or nothing when no handshake ever completed. Nothing, not an empty object:
 * "no TLS" and "TLS with all fields empty" are different sentences, and only the first is ever
 * true here.
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
 * The security headers the panel reports on, present or absent. An allowlist, which the pivot rules
 * refuse and which is right here: the block's value is naming what is missing, and a denylist cannot
 * name an absence. Unlisted headers stay in the raw fold. No score, letter or ranking colour: a grade
 * would be the composite severity forbidden on an asset.
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
 * The internal scripts of a render, where their hashes belong. They left the card (464 badges
 * for 50 rows) and stay a pivot in search and this view. External ones are dropped for the
 * pivot rules' reason: a bundle from a public CDN groups without discriminating.
 */
export function scriptsOf(evidence: Evidence | undefined): Script[] {
	const scripts = list(bag(evidence?.data).scripts)
		.map((raw) => {
			const script = bag(raw);
			return { url: text(script.url), hash: text(script.hash), internal: script.internal === true };
		})
		.filter((script) => script.internal && (script.hash || script.url));

	// Deduplicated here, as nothing else deduplicates the payload: a page loading one bundle from
	// two tags records it twice, and the control plane only collapses them when counting the pivot.
	//
	// Two identical lines are wrong on a counted list anyway, and the screen has a harder reason: a
	// keyed each block over a repeated hash throws in Svelte, so the whole asset view fails to render.
	const seen = new Set<string>();
	return scripts.filter((script) => {
		const key = script.hash ?? script.url ?? '';
		if (seen.has(key)) return false;
		seen.add(key);
		return true;
	});
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
		// Deduplicated like the scripts: normalization sorts this list but does not collapse it, and a
		// repeated host in a keyed each block throws.
		externalHosts: [...new Set(list(data.external_hosts).filter((host): host is string => typeof host === 'string'))]
	};
}

export interface Ports {
	open: number[];
	/** How many ports the scan actually tried, which is what says it ran at all. */
	scanned?: number;
}

/**
 * The ports, counted rather than listed. `evidence.ts` hides `scanned_ports`, `closed_ports`
 * and `filtered_ports` from the raw tree (a hundred numbers identical on every asset are the
 * probe's settings echoed per row). The count is different: it separates "nothing else is open"
 * from "nothing else was tried", which are not the same finding.
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
