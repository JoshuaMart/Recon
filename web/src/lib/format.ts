import type { Asset, Facet, Pivot, Term } from './types';

/**
 * shownFacets drops the facets that have nothing to show. The server sends one per field, and an
 * empty one arrives as `null` (a nil slice in Go); a function, not a template expression, because
 * this took the first render of a real inventory down. `truncated` travels with it: a cut facet must
 * say so, as nine ports that look complete would be a false statement.
 */
export function shownFacets(facets: Facet[]): { field: string; terms: Term[]; truncated: boolean }[] {
	return facets
		.map((facet) => ({
			field: facet.field,
			terms: facet.terms ?? [],
			truncated: facet.truncated ?? false
		}))
		.filter((facet) => facet.terms.length > 0);
}

/**
 * badgesOf is the pivots the server decided are worth a line. The decision is the server's and never
 * re-made here: a second copy of the filter would be a second list to keep in step. `badge` absent
 * (not false) is the export's shape, which never reaches a screen; "not worth one" is safe either way.
 */
export function badgesOf(asset: Asset): Pivot[] {
	return (asset.pivots ?? []).filter((pivot) => pivot.badge === true);
}

/**
 * Relative time in words, for the identity line and the temporal band: "appeared two hours
 * ago" beats "stable for three years" at a glance, which an absolute timestamp does not
 * support. The exact instant belongs in the title attribute.
 */
export function ago(when: string | undefined, now: number = Date.now()): string {
	if (!when) return 'never';
	const parsed = Date.parse(when);
	if (Number.isNaN(parsed)) return 'never';

	const seconds = Math.max(0, (now - parsed) / 1000);
	const day = 86400;
	if (seconds < 90) return 'just now';
	if (seconds < 3600) return `${Math.floor(seconds / 60)} min ago`;
	if (seconds < day) return `${Math.floor(seconds / 3600)} h ago`;
	// Days up to six weeks, not a month: "45 d ago" reads better than "1 month ago" where the two
	// meet, and the temporal band is where someone judges whether to open an asset.
	if (seconds < 45 * day) return `${Math.floor(seconds / day)} d ago`;
	if (seconds < 365 * day) return plural(Math.floor(seconds / (30 * day)), 'month');
	return plural(Math.floor(seconds / (365 * day)), 'year');
}

function plural(count: number, unit: string): string {
	return `${count} ${unit}${count === 1 ? '' : 's'} ago`;
}

/**
 * The sentence a state carries, and its tone. An `unobservable` asset needs an explicit mention, not
 * colour alone, and the two sentences differ in kind: "no observer gets through" is an absence of
 * measurement and licenses no conclusion, "the name no longer resolves" is a measurement. Here, not
 * in the card, as the asset view says the same and two copies would diverge.
 */
export function verdictOf(asset: Asset): { tone: 'unobs' | 'dead'; text: string } | null {
	if (asset.lifecycle === 'unobservable') {
		return {
			tone: 'unobs',
			text: 'No observer gets through. Neither the http probe nor the render obtained anything usable, so the asset is not called dead.'
		};
	}
	if (asset.lifecycle === 'inactive') {
		// The layer verdict, not the lifecycle: a name that no longer resolves and a host whose ports
		// all time out are different findings, and one of the two proves nothing.
		const cause = asset.dns_state === 'dead' ? 'The name no longer resolves' : 'Every probe failed';
		return { tone: 'dead', text: `${cause}, confirmed over more than 24 hours.` };
	}
	if (asset.scope_status === 'unknown') {
		return {
			tone: 'dead',
			text: 'Outside the decided perimeter. No scope rule settles this name, so it is kept and never probed.'
		};
	}
	return null;
}

/**
 * A body size, grouped not abbreviated: `4 637 B`, not `4.6 kB`. The exact number is what is
 * compared against the next observation, and a rounded one hides a body that grew by eighty
 * bytes. Above a megabyte exactness is unreadable, so it rounds.
 */
export function bytes(size: number | undefined): string {
	if (size === undefined || !Number.isFinite(size) || size < 0) return '';
	if (size < 1_000_000) return `${size.toLocaleString('en-GB').replace(/,/g, ' ')} B`;
	return `${(size / 1_000_000).toFixed(1)} MB`;
}

/** Whole days from now until an instant. Negative once it has passed. */
export function daysUntil(when: string | undefined, now: number = Date.now()): number | undefined {
	if (!when) return undefined;
	const parsed = Date.parse(when);
	if (Number.isNaN(parsed)) return undefined;
	return Math.floor((parsed - now) / 86400000);
}

/**
 * How far through a validity window the present sits, as a percentage. Clamped at both ends:
 * an expired certificate draws a full bar, a future one an empty bar, and neither overflows
 * its track.
 */
export function elapsed(from: string | undefined, to: string | undefined, now: number = Date.now()): number {
	if (!from || !to) return 0;
	const start = Date.parse(from);
	const end = Date.parse(to);
	if (Number.isNaN(start) || Number.isNaN(end) || end <= start) return 0;
	return Math.min(100, Math.max(0, Math.round(((now - start) / (end - start)) * 100)));
}

/** The full instant, for a title attribute. */
export function exact(when: string | undefined): string {
	return when ? new Date(when).toISOString().replace('T', ' ').slice(0, 19) + ' UTC' : 'never';
}

/** Which colour family a status code takes. */
export function codeFamily(code: number | undefined): string {
	if (!code) return '';
	return `${Math.floor(code / 100)}xx`;
}

/** The port a scheme implies, dropped from a URL exactly as a canonical key drops it. */
const defaultPorts: Record<string, number> = { https: 443, http: 80 };

/**
 * identity splits the key into the part carrying the meaning and the path. The unit on a card is the
 * service: one whose port determines a scheme is written as the URL someone would open
 * (`https://qui.jomar.ovh`, not `qui.jomar.ovh:443/tcp`); one that never answered over http keeps
 * `host:port`. The path branch survives for the url kind a human declares.
 */
export function identity(asset: Asset): { head: string; path: string } {
	if (asset.kind === 'service') return { head: serviceName(asset), path: '' };
	if (asset.kind !== 'url') return { head: asset.key, path: '' };
	try {
		const url = new URL(asset.key);
		const head = url.origin;
		const path = asset.key.slice(head.length);
		return { head, path: path === '/' ? '/' : path };
	} catch {
		return { head: asset.key, path: '' };
	}
}

/**
 * The URL a service is reached at, or "" when nothing established one. The scheme is the one the
 * probe measured (the projection), never inferred from the port: a TLS listener on 8080 and a plain
 * one on 8443 both exist. The open link uses it too, so a card never builds a second spelling of an
 * address.
 */
export function serviceURL(asset: Asset): string {
	if (!asset.scheme || !asset.host) return '';
	const port = asset.port;
	const authority = port && port !== defaultPorts[asset.scheme] ? `${asset.host}:${port}` : asset.host;
	return `${asset.scheme}://${authority}`;
}

/**
 * How a service is written: the URL it answers at, or `host:port` when no probe made it
 * answer. `/tcp` is dropped and other protocols kept: the canonical key spells it out for
 * unambiguity, but a list of web services need not say "TCP" a hundred times, and a udp
 * service is told apart by its suffix.
 */
function serviceName(asset: Asset): string {
	const url = serviceURL(asset);
	if (url) return url;

	const slash = asset.key.lastIndexOf('/');
	const authority = slash >= 0 ? asset.key.slice(0, slash) : asset.key;
	const proto = slash >= 0 ? asset.key.slice(slash + 1) : 'tcp';
	return proto === 'tcp' ? authority : `${authority}/${proto}`;
}

/**
 * Where a service landed, or "" when it landed on itself. The path alone when the host did not change
 * (the ordinary redirect to a login page); the whole URL when it did, as a service sending elsewhere is
 * a different fact. A function so its breaking cases (no final URL, one equal to the base, one that will
 * not parse) can be held by a test.
 */
export function landingOf(asset: Asset, head: string): string {
	const target = asset.final_url;
	if (!target) return '';
	try {
		const url = new URL(target);
		const base = new URL(head.includes('://') ? head : 'https://' + head);
		if (url.host !== base.host) return target;
		const rest = url.pathname + url.search;
		return rest === '/' ? '' : rest;
	} catch {
		// An unparseable value is still worth showing: it is what the probe recorded,
		// and hiding it would make a malformed Location header invisible.
		return target;
	}
}

/**
 * The last step of the lineage, the one that produced this asset. The chain is written by
 * whatever discovered it, each entry an object with a step name beside whatever else that
 * producer recorded. Only the name is read; the rest belongs on the asset view.
 */
export function lineage(asset: Asset): string {
	const path = asset.lineage;
	if (path && path.length > 0) {
		const step = path[path.length - 1]?.step;
		if (step) return step;
	}
	if (asset.discovery_source) return 'found by ' + asset.discovery_source;
	return 'no recorded lineage';
}

/**
 * The right-hand label of a row, not always a status code. Inline in the card until the card
 * became a row and the asset view needed the same answer. Nothing probes a name over http, so
 * "no answer" would answer a question nobody asked; a name is measured on its dns state.
 */
export function statusLabel(asset: Asset): { code: string; family: string } {
	if (asset.status_code) return { code: String(asset.status_code), family: codeFamily(asset.status_code) };
	if (asset.dns_state === 'dead') return { code: 'does not resolve', family: '' };
	if (asset.scope_status === 'unknown') return { code: 'never probed', family: '' };
	if (!asset.last_checked_at) return { code: 'never probed', family: '' };
	if (!webSurface(asset)) return { code: asset.dns_state ? 'dns ' + asset.dns_state : 'not measured', family: '' };
	return { code: 'no answer', family: '' };
}

/**
 * webSurface says whether this kind is probed over http at all. A name and an address are
 * not: their services are, and they carry only dns and tcp layers. Anything a card writes
 * about a response (status code, render, cookie) is about a question never asked of them,
 * and "no answer" to it is the kind of false statement this interface exists to avoid.
 */
export function webSurface(asset: Asset): boolean {
	return asset.kind === 'service' || asset.kind === 'url';
}

/**
 * cookieState says which of the three cases a card is in. The third is why this is not a
 * check on the badges: an asset setting only PHPSESSID has been rendered and does set
 * cookies, so "no cookies" would be false. A missing badge must say which of the three it is.
 */
export type CookieState = 'never-rendered' | 'none' | 'all-filtered' | 'shown';

export function cookieState(asset: Asset): CookieState {
	if (!asset.last_fingerprint_at) return 'never-rendered';
	if (badgesOf(asset).some((pivot) => pivot.type === 'cookie_name')) return 'shown';
	const names = asset.attributes?.cookie_names;
	if (names && names.length > 0) return 'all-filtered';
	return 'none';
}

/**
 * pivotOf finds one pivot on an asset, whatever the row decided about it. All of them, not
 * only the badge-worthy, unlike `badgesOf`: a denylisted cookie name or a script hash is
 * still a pivot with a counter. The badge went because a line read in under a second cannot
 * hold twelve, but this view shows one asset, where twelve counted rows are the right grain.
 */
export function pivotOf(asset: Asset, type: Pivot['type'], value?: string): Pivot | undefined {
	return (asset.pivots ?? []).find((pivot) => pivot.type === type && (value === undefined || pivot.value === value));
}

/**
 * cardHashes is the hash family a row may show, deliberately two entries. Script hashes are not
 * badges: a real inventory produced 464 across 50 rows, a granularity error, since a badge must fit a
 * line read in under a second. Nothing leaves collection or search: the hash stays filterable through
 * `script_hash`, counted, and listed on the asset view where twelve lines are readable.
 */
export function cardHashes(asset: Asset): Pivot[] {
	return badgesOf(asset).filter((pivot) => pivot.type === 'favicon' || pivot.type === 'cert_spki');
}

/**
 * infraState is the three-way answer for the infrastructure family. The first case matters:
 * a deployment without a MaxMind database is normal, and an empty family there reads as a
 * broken interface. The console cannot tell "not configured" from "configured, no match"
 * (both give zero ASN), so the server says which, on GET /assets/fields.
 */
export type InfraState = 'unconfigured' | 'cdn' | 'no-match' | 'shown';

export function infraState(asset: Asset, enrichmentConfigured: boolean): InfraState {
	if (!enrichmentConfigured) return 'unconfigured';
	if (asset.is_cdn) return 'cdn';
	if (!asset.asn && !asset.country) return 'no-match';
	return 'shown';
}

/**
 * geoVisible is the geolocation rule, written as a rule and not a comment. On a fronted target
 * the address is a point of presence, so the city says where the CDN is, not the asset. The
 * ASN stays: it remains the most actionable line of the infrastructure family.
 */
export function geoVisible(asset: Asset): boolean {
	return asset.is_cdn !== true && Boolean(asset.country);
}

/** The version a technology carries, from the object beside the column. */
export function versionOf(asset: Asset, name: string): string | undefined {
	return asset.attributes?.technologies?.find((tech) => tech.name === name)?.version;
}

/**
 * The flag of a two-letter country code, built from regional indicator letters (two code points, no
 * asset or request); a country nobody can render still reads as its code. Only called where the
 * country may be shown, never on a fronted asset, where the address is a point of presence and the
 * flag would name the wrong country.
 */
export function flag(country: string | undefined): string {
	if (!country || country.length !== 2 || !/^[a-z]{2}$/i.test(country)) return '';
	const base = 0x1f1e6 - 'A'.charCodeAt(0);
	return String.fromCodePoint(...[...country.toUpperCase()].map((letter) => letter.charCodeAt(0) + base));
}
