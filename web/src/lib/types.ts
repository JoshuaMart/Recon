export interface Row {
	asset_id: string;
	program_id: string;
	program: string;
	kind: string;
	key: string;
	host: string;
	port: number | null;
	scheme: string | null;
	scope: string;
	lifecycle: string;
	status_code: number | null;
	title: string | null;
	server: string | null;
	final_url: string | null;
	cdn: string | null;
	waf: string | null;
	country: string | null;
	technologies: string[];
	thumbnail: string | null;
	triage: string;
	first_seen: string;
	last_seen: string;
	last_changed: string | null;
	services?: string[];
}

export interface Page {
	query: string;
	view: string;
	rows: Row[];
	next?: string;
	limit: number;
}

export interface FacetValue {
	value: string;
	count: number;
	term: string;
}

export interface Program {
	id: string;
	origin: string;
	platform: string | null;
	slug: string | null;
	name: string;
	bounty: boolean | null;
	state: string;
	archived_by: string | null;
	auto_scan: string | null;
	effective_auto_scan: string;
	created_at: string;
	updated_at: string;
	open_rules?: number;
	wildcards?: number;
}

export interface Rule {
	id: string;
	kind: string;
	matcher: string;
	pattern: string;
	origin: string;
	note: string | null;
	valid_from: string;
	valid_to: string | null;
	in_force: boolean;
}

export interface Wildcard {
	id: string;
	program_id: string;
	program_name: string;
	platform: string | null;
	apex: string;
	active: boolean;
	auto_scan: string;
	last_recon_at: string | null;
	assets_found: number;
	created_at: string;
}

export interface Run {
	id: string;
	program_id: string;
	kind: string;
	stage: string;
	wildcard_id: string | null;
	apex?: string;
	state: string;
	trigger: string;
	queued_at: string;
	started_at: string | null;
	finished_at: string | null;
	error: string | null;
	report?: Record<string, unknown>;
	ingest?: Record<string, unknown>;
}

export interface Change {
	id: number;
	asset_id: string;
	program_id: string;
	program: string;
	asset_kind: string;
	asset_key: string;
	kind: string;
	layer: string | null;
	before: unknown;
	after: unknown;
	run_id: string | null;
	at: string;
}

export interface SavedSearch {
	id: string;
	name: string;
	query: string;
	view: string;
	alert: boolean;
	created_at: string;
	updated_at: string;
}

export interface Triage {
	asset_id: string;
	state: string;
	tags: string[];
	note: string | null;
	seen_at: string | null;
}
