/**
 * What a host header may say about its services. `Group` carries no aggregate, so everything the
 * header shows is derived here under one rule: a value rises to the header only when the services
 * carrying it agree, and stays on the rows once they diverge. Five ports of one address usually share
 * an operator, city and certificate, but a host with two A records would otherwise state one member's
 * value for all; a header right nine times in ten is worse than none.
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
 * agreed returns the single value the members carry, or nothing. Members carrying no value do
 * not vote: an fqdn and an http-only service have no certificate, and letting their absence veto
 * would hide a certificate every presenting service agrees on.
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
		// A single `false` among the members is a disagreement like any other, so the question is asked of
		// the fronted assets, not all: geolocation is dropped only where the address is a point of presence.
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
 * The favicon the header shows when services do not agree. The agreement rule is right about what the
 * header may claim and wrong about what it may show: four applications got an empty square. The front
 * door is the service someone would open at that name (https on 443, then http on 80, then whatever
 * answered first), a rule and not a pick; rows keep their own badge and counter because they differ.
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
 * What a row shows of its own pivots once the header has spoken. A favicon or certificate the whole
 * host shares is stated once above; one that differs is the line unlike the others, so it stays.
 * Cookie names always stay: per service by nature, and their three absences are what the row keeps
 * saying.
 */
export function rowBadges(asset: Asset, shared: Shared): Pivot[] {
	return badgesOf(asset).filter((pivot) => {
		if (pivot.type === 'favicon') return pivot.value !== shared.favicon;
		if (pivot.type === 'cert_spki') return pivot.value !== shared.cert?.value;
		// A script pivot arrives on the row with its counter, never as a badge. The rule is here as well
		// as on the server because a badge has to fit a line read in under a second.
		return pivot.type === 'cookie_name';
	});
}

/**
 * The services of a group, and the asset the header is. The fqdn or ip of a host folds into the
 * header, not beside its services: its status code repeats its `:443` service's answer. It is
 * still probed and one click away through the header.
 */
export function splitGroup(group: Group): { self?: Asset; services: Asset[] } {
	const self = group.assets.find((asset) => asset.kind === 'fqdn' || asset.kind === 'ip');
	return { self, services: group.assets.filter((asset) => asset !== self) };
}
