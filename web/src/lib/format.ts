import type { Asset, Facet, Pivot, Term } from './types';

/**
 * shownFacets drops the facets that have nothing to show.
 *
 * The server answers with one facet per field whether or not the filtered set has
 * a value for it, and an empty one arrives as `null` because a nil slice encodes
 * that way in Go. This is a function rather than an expression in a component
 * because it is what took the first render of a real inventory down: the type
 * said `Term[]`, the wire said `null`, and nothing sat between the two.
 *
 * `truncated` travels with it. A facet that was cut has to say so, for the reason
 * the export refuses to truncate silently: a list of nine ports that looks
 * complete is a statement about the inventory, and it is false.
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
 * badgesOf is the pivots the server decided are worth a line.
 *
 * The decision is the server's and this never re-makes it. A counter of one is a
 * pivot leading only to itself and a value on the denylist groups without
 * discriminating, and both filters live in the search layer: a second copy here
 * would be a second list to keep in step, and the divergence would read as a badge
 * appearing on one screen and not the other.
 *
 * `badge` absent rather than false is the export's shape, which never reaches a
 * screen. Treating it as "not worth one" is the safe reading either way.
 */
export function badgesOf(asset: Asset): Pivot[] {
	return (asset.pivots ?? []).filter((pivot) => pivot.badge === true);
}

/**
 * Relative time, in words, for the identity line and the temporal band.
 *
 * "An asset that appeared two hours ago is more interesting than one
 * stable for three years", which is a comparison a reader makes at a glance and
 * an absolute timestamp does not support. The exact instant belongs in the title
 * attribute, where it is one hover away.
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
	// Days up to six weeks rather than up to a month. "45 d ago" reads better
	// than "1 month ago" at the point where the two meet, and the temporal band
	// is where somebody judges whether an asset is worth opening.
	if (seconds < 45 * day) return `${Math.floor(seconds / day)} d ago`;
	if (seconds < 365 * day) return plural(Math.floor(seconds / (30 * day)), 'month');
	return plural(Math.floor(seconds / (365 * day)), 'year');
}

function plural(count: number, unit: string): string {
	return `${count} ${unit}${count === 1 ? '' : 's'} ago`;
}

/**
 * The sentence a state carries, and the tone it carries it in.
 *
 * An `unobservable` asset needs an explicit mention rather than a colour alone, and
 * the three absences says why the two sentences differ in kind: "no observer gets through" is an
 * absence of measurement, "the name no longer resolves" is a measurement. The first
 * licenses no conclusion about the asset.
 *
 * Here rather than in the card because the asset view says the same thing about the
 * same asset, and two copies of a rule this specific diverge on the day somebody edits
 * one of them.
 */
export function verdictOf(asset: Asset): { tone: 'unobs' | 'dead'; text: string } | null {
	if (asset.lifecycle === 'unobservable') {
		return {
			tone: 'unobs',
			text: 'No observer gets through. Neither the http probe nor the render obtained anything usable, so the asset is not called dead.'
		};
	}
	if (asset.lifecycle === 'inactive') {
		// The layer verdict rather than the lifecycle, because the two sentences
		// are different findings: a name that no longer resolves and a host whose
		// ports all time out are not the same, and one of the two proves nothing.
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
 * A body size, grouped rather than abbreviated.
 *
 * `4 637 B` and not `4.6 kB`: the exact number is what somebody compares against the
 * next observation of the same asset, and a rounded one hides a body that grew by
 * eighty bytes. Above a megabyte the exactness stops being readable, so it rounds.
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
 * How far through a validity window the present moment sits, as a percentage.
 *
 * Clamped at both ends rather than left to overflow: a certificate whose window has
 * passed draws a full bar, one issued in the future draws an empty one, and neither
 * draws a bar longer than its track.
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
 * identity splits the key into the part that carries the meaning and the path.
 *
 * The unit on a card is the service , in the database as well as on the
 * screen since the identity rule. A service whose port determines a scheme is written as the
 * URL somebody would open — `https://qui.jomar.ovh` rather than
 * `qui.jomar.ovh:443/tcp` — and one that has never answered over http keeps the
 * `host:port` form, because nothing has established that a browser belongs there.
 *
 * The path branch survives for the url kind, which the identity rule keeps legal for what a
 * human declares.
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
 * The URL a service is reached at, or an empty string when nothing established one.
 *
 * The scheme is the one the probe **measured** (the projection), never one inferred from
 * the port: a TLS listener on 8080 and a plain one on 8443 both exist, so the number
 * says nothing and the request says everything. Before the column existed, thirty
 * services on a real inventory answered while the card refused to say how to reach
 * them — and there was no filter for "what can I open", because the distinction was
 * drawn in the interface and nowhere else.
 *
 * It is also what the open link uses, which is why it returns a URL rather than a
 * label: a card must never build a second spelling of the same address.
 */
export function serviceURL(asset: Asset): string {
	if (!asset.scheme || !asset.host) return '';
	const port = asset.port;
	const authority = port && port !== defaultPorts[asset.scheme] ? `${asset.host}:${port}` : asset.host;
	return `${asset.scheme}://${authority}`;
}

/**
 * How a service is written: the URL it answers at, or `host:port` when no probe made
 * it answer.
 *
 * `/tcp` is dropped and any other protocol is kept. The canonical key spells the
 * protocol out because it has to be unambiguous; a reader looking at a list of web
 * services does not need to be told a hundred times that they are TCP, and the day
 * a udp service appears the suffix is what says so.
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
 * Where a service landed, or an empty string when it landed on itself (the projection).
 *
 * The path alone when the host did not change, which is the ordinary redirect to a
 * login page: repeating the host a reader has just read two centimetres to the left
 * one zone echoing another. The whole URL when the host did change,
 * because a service that sends somewhere else is a different fact and the host is
 * the half of it that matters.
 *
 * A function here rather than an expression in the card, for the reason shownFacets
 * is one: the cases that break it — no final URL, a final URL equal to the base, a
 * URL that will not parse — are cases a test can hold and a template cannot.
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
 * The last step of the lineage, which is the one that produced this asset.
 *
 * The chain is written by whatever discovered it, and each entry is an object
 * carrying a step name beside whatever else that producer recorded. Only the name
 * is read here; the rest belongs on the asset view, which has room for it.
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
 * The right-hand label of a row, which is not always a status code (the fold).
 *
 * It was inline in the card until the card became a row and the asset view started
 * needing the same answer. The cases are what it exists for: nothing probes a name
 * over http, so "no answer" would answer a question nobody asked, and its dns state is
 * what a name is measured on.
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
 * webSurface says whether this kind is probed over http at all .
 *
 * A name and an address are not web surfaces: their services are, and since the probe stopped asking them
 * they carry the dns and tcp layers and nothing else. Everything a card writes about
 * a response — a status code, a render, a cookie — is a sentence about a question
 * that is never asked of them, and "no answer" to a question nobody asked is the
 * kind of false statement this interface exists to avoid.
 */
export function webSurface(asset: Asset): boolean {
	return asset.kind === 'service' || asset.kind === 'url';
}

/**
 * cookieState says which of the three cases a card is in (the three absences).
 *
 * The third one is why this function exists rather than a check on the badges:
 * an asset that only sets PHPSESSID has been rendered and does set cookies, and
 * showing it as "no cookies" would state something false. A missing badge has to
 * say which of the three it is.
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
 * pivotOf finds one pivot on an asset, whatever the row decided about it.
 *
 * All of them and not only the ones worth a badge, and that is the difference
 * between this and `badgesOf`. A denylisted cookie name and a script hash are
 * still pivots with their counters: the badge went because a line read in under a
 * second cannot hold twelve of them, and this view shows one asset, where twelve
 * counted rows are exactly the right granularity.
 */
export function pivotOf(asset: Asset, type: Pivot['type'], value?: string): Pivot | undefined {
	return (asset.pivots ?? []).find((pivot) => pivot.type === type && (value === undefined || pivot.value === value));
}

/**
 * cardHashes is the hash family a row may show, and it is deliberately two
 * entries.
 *
 * Script hashes are not badges. A real inventory produced 464 hash badges across
 * 50 rows, and that is a granularity error rather than a missing cap: a badge has
 * to fit in a line somebody reads in under a second. A counter threshold does no
 * better, because the counter of one case is already filtered and what remains is
 * real sharing, so the threshold would have to be a function of program size.
 *
 * Nothing is removed from collection or from search: the same hash is still
 * filterable through `script_hash`, still counted, and still on the asset view as
 * a counted list, where twelve lines are readable.
 */
export function cardHashes(asset: Asset): Pivot[] {
	return badgesOf(asset).filter((pivot) => pivot.type === 'favicon' || pivot.type === 'cert_spki');
}

/**
 * infraState is the three absences's three-way answer for the infrastructure family.
 *
 * The case that matters is the first one. A deployment with no MaxMind database
 * is a normal deployment , and showing an empty family there reads as a
 * broken interface. The console cannot work this out from the data — "not
 * configured" and "configured, no match" both give zero ASN — so the server says
 * which, on GET /assets/fields.
 */
export type InfraState = 'unconfigured' | 'cdn' | 'no-match' | 'shown';

export function infraState(asset: Asset, enrichmentConfigured: boolean): InfraState {
	if (!enrichmentConfigured) return 'unconfigured';
	if (asset.is_cdn) return 'cdn';
	if (!asset.asn && !asset.country) return 'no-match';
	return 'shown';
}

/**
 * geoVisible is the geolocation rule, written as a rule rather than as a comment.
 *
 * On a fronted target the address is that of a point of presence, so the city
 * says where the CDN is and not where the asset is. The ASN stays, because it
 * remains the most actionable line of the infrastructure family.
 */
export function geoVisible(asset: Asset): boolean {
	return asset.is_cdn !== true && Boolean(asset.country);
}

/** The version a technology carries, from the object beside the column. */
export function versionOf(asset: Asset, name: string): string | undefined {
	return asset.attributes?.technologies?.find((tech) => tech.name === name)?.version;
}

/**
 * The flag of a two-letter country code.
 *
 * Built from the regional indicator letters rather than shipped as images: it is two
 * code points, it needs no asset and no request, and it renders in the font the page
 * already has. A country nobody can render still reads as its code.
 *
 * It is only ever called where the country may be shown at all, never on a
 * fronted asset, where the address is a point of presence and the flag would name the
 * wrong country with more confidence than the code did.
 */
export function flag(country: string | undefined): string {
	if (!country || country.length !== 2 || !/^[a-z]{2}$/i.test(country)) return '';
	const base = 0x1f1e6 - 'A'.charCodeAt(0);
	return String.fromCodePoint(...[...country.toUpperCase()].map((letter) => letter.charCodeAt(0) + base));
}
