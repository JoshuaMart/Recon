/**
 * What a host header may say about its services.
 *
 * `Group` is `{host, last_seen, assets}` and carries no aggregate, so everything
 * the header shows is derived here. The rule that makes it honest is the whole
 * content of this module: a value rises to the header only when the services that
 * carry it agree on one value, and it stays on the rows the moment they diverge.
 *
 * That is not a precaution of style. Five ports of one address usually do share an
 * operator, a city and a certificate, which is why the header is worth having, but
 * a host with two A records, or a fronted service beside a direct one, would
 * otherwise make the header state one member's value for all of them. A header
 * that is right nine times out of ten is worse than no header, because nothing on
 * screen says which time this is.
 */

import type { Asset, Group, Pivot } from './types';
import { badgesOf, lineage } from './format';

export interface Shared {
	asn?: number;
	asnOrg?: string;
	country?: string;
	city?: string;
	ip?: string;
	isCdn?: boolean;
	cdnProvider?: string;
	/** The certificate pivot, when every service that presents one presents the same. */
	cert?: Pivot;
	/** The favicon hash of the host, and the image key the page answers with. */
	favicon?: string;
	/** The last step of the lineage, when it is the same step for the whole group. */
	lineage?: string;
}

/**
 * agreed returns the single value the members carry, or nothing.
 *
 * Members that carry no value do not vote: an fqdn has no certificate and an http-only
 * service has none either, and letting their absence veto the header would hide a
 * certificate that every service presenting one agrees on.
 */
function agreed<T>(assets: Asset[], read: (asset: Asset) => T | undefined): T | undefined {
	let found: T | undefined;
	for (const asset of assets) {
		const value = read(asset);
		if (value === undefined || value === null || value === '') continue;
		if (found === undefined) found = value;
		else if (found !== value) return undefined;
	}
	return found;
}

function badgeOf(asset: Asset, type: Pivot['type']): Pivot | undefined {
	return badgesOf(asset).find((pivot) => pivot.type === type);
}

/** sharedOf reads what the header of one group may state. */
export function sharedOf(group: Group): Shared {
	const assets = group.assets;
	const certValue = agreed(assets, (asset) => badgeOf(asset, 'cert_spki')?.value);

	return {
		asn: agreed(assets, (asset) => asset.asn),
		asnOrg: agreed(assets, (asset) => asset.asn_org),
		country: agreed(assets, (asset) => asset.country),
		city: agreed(assets, (asset) => asset.city),
		ip: agreed(assets, (asset) => asset.ip),
		// A single `false` among the members is a disagreement like any other, so
		// the question is asked of the assets that are fronted rather than of all
		// of them: the geolocation is only dropped where the address is a point of
		// presence.
		isCdn: agreed(assets, (asset) => (asset.is_cdn ? true : undefined)),
		cdnProvider: agreed(assets, (asset) => asset.cdn_provider),
		cert: certValue
			? assets.map((asset) => badgeOf(asset, 'cert_spki')).find((badge) => badge?.value === certValue)
			: undefined,
		favicon: agreed(assets, (asset) => asset.attributes?.favicon_hash),
		lineage: agreed(assets, (asset) => lineage(asset))
	};
}

/**
 * The favicon the header shows when the services do not agree on one.
 *
 * The agreement rule is right about what the header may **claim** and wrong about what
 * it may **show**: a host serving four applications got an empty square, which reads as
 * a missing image rather than as four different icons. The two questions are not the
 * same, so they stop sharing an answer here.
 *
 * The front door is the service somebody would open at that name: the https one on 443,
 * then the http one on 80, then whatever answered first. It is a rule and not a pick,
 * which is what separates it from stating one member's value for all of them — and the
 * rows keep their own favicon badge, with its counter, precisely because they differ.
 */
export function frontDoorFavicon(group: Group): string | undefined {
	const hashOf = (asset: Asset) => asset.attributes?.favicon_hash;
	const candidates = [
		group.assets.find((asset) => asset.scheme === 'https' && asset.port === 443 && hashOf(asset)),
		group.assets.find((asset) => asset.scheme === 'http' && asset.port === 80 && hashOf(asset)),
		group.assets.find((asset) => (asset.kind === 'fqdn' || asset.kind === 'ip') && hashOf(asset)),
		group.assets.find((asset) => hashOf(asset))
	];
	return candidates.find(Boolean) ? hashOf(candidates.find(Boolean)!) : undefined;
}

/**
 * What a row shows of its own pivots, once the header has spoken.
 *
 * A favicon or a certificate the whole host shares is stated once above, and
 * repeating it on every line is the duplication the fold exists to remove. One that
 * differs is the opposite of noise, since it is the line that is not like the
 * others, so it stays.
 *
 * Cookie names always stay. They are per service by nature, and their three
 * absences are what the row has to keep saying.
 */
export function rowBadges(asset: Asset, shared: Shared): Pivot[] {
	return badgesOf(asset).filter((pivot) => {
		if (pivot.type === 'favicon') return pivot.value !== shared.favicon;
		if (pivot.type === 'cert_spki') return pivot.value !== shared.cert?.value;
		// A script pivot arrives on the row with its counter and never as a badge.
		// The rule is here as well as on the server because a row is what it
		// exists to protect: a badge has to fit in a line read in under a second.
		return pivot.type === 'cookie_name';
	});
}

/**
 * The services of a group, and the asset the header is.
 *
 * The fqdn or the ip of a host is folded into the header rather than shown beside
 * its own services: it carries a status code that repeats the answer of its `:443`
 * service. It is still probed and still one click away, through the header.
 */
export function splitGroup(group: Group): { self?: Asset; services: Asset[] } {
	const self = group.assets.find((asset) => asset.kind === 'fqdn' || asset.kind === 'ip');
	return { self, services: group.assets.filter((asset) => asset !== self) };
}
