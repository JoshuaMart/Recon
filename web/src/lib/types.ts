/**
 * The shapes internal/search and internal/api return, mirrored field by field.
 *
 * Hand written rather than generated, and the reason to say so is that nothing
 * checks the agreement today: a field renamed in Go would arrive here as
 * `undefined` and render as an empty badge rather than as a failure. The day this
 * drifts, the pattern to copy is the contract test that pins the fingerprinter's
 * response.
 */

/** One pivot value, with what it links and whether it earns a badge. */
export interface Pivot {
	type: 'favicon' | 'script' | 'cert_spki' | 'cookie_name';
	value: string;
	/** Includes the asset showing it. */
	count: number;
	/**
	 * Display only, and absent entirely on the export.
	 *
	 * The server decides it, and the console never re-decides it: a value the
	 * denylist names or one leading only to itself is not worth a line in a row
	 * scanned in under a second. It stays fully searchable and fully counted,
	 * which is the difference between removing a badge and removing data.
	 *
	 * Optional rather than boolean, because a file that answered `false` could
	 * not be told from one that never asked the question.
	 */
	badge?: boolean;
}

/** One step of the lineage, written by whatever discovered the asset. */
export interface Step {
	step?: string;
	run?: string;
	host?: string;
	port?: number;
	sources?: string[];
	addresses?: string[];
}

export interface Asset {
	asset_id: string;
	program_id: string;
	kind: 'fqdn' | 'ip' | 'service' | 'url';
	key: string;
	scope_status: 'in_scope' | 'out_of_scope' | 'unknown';

	lifecycle: 'candidate' | 'active' | 'flapping' | 'inactive' | 'unobservable' | 'archived';
	/** Death is a property of a layer, so the three verdicts travel separately. */
	dns_state?: 'unmeasured' | 'healthy' | 'failing' | 'dead';
	tcp_state?: 'unmeasured' | 'healthy' | 'failing' | 'dead';
	http_state?: 'unmeasured' | 'healthy' | 'failing' | 'dead';

	ip?: string;
	port?: number;
	status_code?: number;
	/** The status of every hop, in order. One entry is not a chain. */
	status_chain?: number[];
	/** Where the last hop landed, shown only when it differs from the base URL. */
	final_url?: string;
	/** How the service is addressed, measured by the probe rather than inferred. */
	scheme?: string;
	/** The host of the asset, derived from its canonical key at creation. */
	host?: string;
	title?: string;
	server?: string;
	asn?: number;
	asn_org?: string;
	country?: string;
	city?: string;
	is_cdn?: boolean;
	cdn_provider?: string;
	waf_detected?: boolean;
	waf_vendor?: string;

	technologies: string[];
	attributes: Attributes;
	/** Changes over the last seven days, summed from the daily buckets. */
	volatility: number;
	pivots?: Pivot[];

	discovery_source: string;
	/** The producer's own chain, newest step last. */
	lineage?: Step[];

	first_seen: string;
	last_seen: string;
	last_checked_at?: string;
	last_changed_at?: string;
	/**
	 * Null means the fingerprinter has never rendered this asset, which is what
	 * separates "no cookie" from "never rendered" on a row.
	 */
	last_fingerprint_at?: string;
}

/**
 * The projected pivots, plus what the http layer contributes.
 *
 * `technologies` here carries the versions, while the promoted column carries the
 * bare names: the column is the index and the facet, the object is the evidence a
 * row shows.
 */
export interface Attributes {
	favicon_hash?: string;
	cert_spki_hash?: string;
	script_hashes?: string[];
	cookie_names?: string[];
	external_hosts?: string[];
	dead_external_hosts?: string[];
	technologies?: { name: string; version?: string }[];
	waf_source?: string;
	origin_error?: string;
	takeover_candidate?: {
		kind?: string;
		target?: string;
		signature?: string;
		detected_at?: string;
	};
}

/** One host and the assets of the result that belong to it. */
export interface Group {
	host: string;
	last_seen: string;
	assets: Asset[];
}

/** What POST /assets/hosts answers. */
export interface GroupedPage {
	groups: Group[];
	/**
	 * The images of the page's favicons, keyed by hash.
	 *
	 * On the page and not on each asset: a shared favicon is the interesting
	 * case, and repeating two kilobytes per asset would undo the point of storing
	 * one copy.
	 */
	favicons?: Record<string, string>;
	/** Empty on the last page. There is deliberately no total. */
	next_cursor?: string;
}

/** What POST /assets/search answers. The flat shape, which the export walks. */
export interface FlatPage {
	assets: Asset[];
	next_cursor?: string;
}

export interface Term {
	value: string;
	count: number;
}

export interface Facet {
	field: string;
	/**
	 * Null when the facet has no term, not an empty array.
	 *
	 * A nil slice encodes as `null` in Go, and the server returns one facet per
	 * field whether or not the filtered set has a value for it. Typing this as
	 * `Term[]` is what took the first render of a real inventory down with
	 * "cannot read properties of null": the type said one thing and the wire said
	 * another, and nothing sat between the two.
	 */
	terms: Term[] | null;
	/** Values were left out. A truncated facet that looks complete makes somebody
	 *  believe the inventory holds nine ports. */
	truncated?: boolean;
}

/**
 * What GET /assets/fields answers: the searchable vocabulary, and what this
 * deployment can do.
 *
 * `enrichment` is optional so that a console pointed at an older control plane
 * degrades to "not configured" rather than to `undefined.configured`.
 */
export interface Capabilities {
	/** Field to the operators it accepts, so the sidebar does not guess one. */
	fields: Record<string, string[]>;
	facets?: number;
	page_limit?: number;
	page_default?: number;
	enrichment?: { configured: boolean };
}

/** One changed field, as internal/diff renders it. */
export interface DiffField {
	field: string;
	kind: 'added' | 'removed' | 'replaced' | 'appeared' | 'disappeared';
	before?: unknown;
	after?: unknown;
	added?: string[];
	removed?: string[];
}

export interface Diff {
	/**
	 * The Notifier's reading, not a second one. A revelation is not an alert:
	 * `detection_improved` means the observer sees better and the target did not
	 * move.
	 */
	class: 'real_change' | 'detection_improved';
	previous_producer_version?: string;
	fields: DiffField[];
}

/** The last observation of one layer, whole. */
export interface Evidence {
	layer: 'dns' | 'tcp' | 'http' | 'fingerprint';
	outcome: 'ok' | 'fail' | 'error';
	source: string;
	/** When this state began. */
	observed_at: string;
	/** The last probe that found it unchanged. */
	last_confirmed_at: string;
	producer_version?: string;
	data: Record<string, unknown>;
}

/**
 * One entry of the timeline: a state, how long it held, and what moved on the way
 * in.
 *
 * `diff` absent on the oldest entry read for a layer means "not compared", which
 * is not "nothing changed". The screen has to say which.
 */
export interface Change {
	layer: 'dns' | 'tcp' | 'http' | 'fingerprint';
	at: string;
	held_until: string;
	outcome: string;
	producer_version?: string;
	diff?: Diff;
}

/** What GET /assets/{id} answers. */
export interface Detail {
	asset: Asset;
	evidence: Evidence[];
	timeline: Change[];
	/** The layers the cap cut, named. The window is never announced. */
	truncated_layers?: string[];
	window_from: string;
	favicons?: Record<string, string>;
}

/** One line of the live feed: what appeared, and why. */
export interface Discovery {
	asset_id: string;
	program_id: string;
	kind: string;
	key: string;
	host?: string;
	lifecycle: string;
	scope_status: string;
	first_seen: string;
	discovery_source: string;
	/** The last step of the lineage, which is the one that produced this asset. */
	step?: string;
}

export interface FeedTick {
	discoveries: Discovery[];
	/** What the cap left out, answered rather than dropped. */
	overflow?: number;
	/** The count itself was capped, so the number above is a floor. */
	overflow_at_least?: boolean;
	cursor: string;
}

/** A perimeter, as a screen reads it. */
export interface Program {
	id: string;
	name: string;
	platform?: string;
	platform_ref?: string;
	state: 'active' | 'suspended' | 'archived';
	authorized_from: string;
	authorized_to?: string;
	authorization_ref?: string;
	rate_limit_rps: number;
	discovery_interval: string;
	last_discovery_at?: string;
	/** What a write has to carry back. */
	version: number;
	created_at: string;
	updated_at: string;
	rules_in_force?: number;
	/** Present only when the list was asked for the counters. */
	assets?: number;
	assets_in_scope?: number;
}

export interface ScopeRule {
	id: string;
	kind: 'include' | 'exclude';
	matcher: 'apex' | 'fqdn' | 'cidr' | 'regex' | 'url_prefix';
	pattern: string;
	valid_from: string;
	valid_to?: string;
	note?: string;
	version: number;
	created_at: string;
	/** The window, answered by the server so no screen reimplements it. */
	in_force: boolean;
}

export interface ProgramDetail {
	program: Program;
	rules: ScopeRule[];
}

/** What a scope write moved, which commits with it. */
export interface Effect {
	examined: number;
	changed: number;
	gained: number;
	lost: number;
}

/**
 * The queue, as the control plane answers it.
 *
 * The three counts are disjoint: a row held by a run is not also due. A pair whose
 * three numbers are zero is dropped, so an entry here is always a queue that
 * exists. Rows with no due date are counted nowhere, because a null is how a row
 * leaves the scheduler.
 */
export interface QueueDepth {
	program_id: string;
	queue: 'resolve' | 'full' | 'fingerprint';
	/** Its slot has come and nothing holds it. */
	due: number;
	/** Scheduled, not yet due. */
	later: number;
	/** Listed in a run that has not finished. */
	in_run: number;
}

export interface Run {
	id: string;
	program_id: string;
	kind: 'discovery' | 'verification';
	scope: 'enum' | 'resolve' | 'ports' | 'full';
	state: 'pending' | 'running' | 'completed' | 'failed' | 'expired';
	deadline: string;
	created_at: string;
	/** Written the first time a scanner reached for the run. Absent means nothing
	 *  claimed it, which calls for the opposite action to a run in flight. */
	started_at?: string;
	finished_at?: string;
	target_count?: number;
	observations: number;
	error?: string;
	/**
	 * What the platform called the execution.
	 *
	 * Absent means nothing started it, which is not the same as a run no scanner
	 * has opened yet: the first is a provisioning that failed, the second is a
	 * run to wait for. It is also the only way to find the execution's logs.
	 */
	external_id?: string;
}

export interface QueueView {
	depths: QueueDepth[];
	runs: Run[];
}
