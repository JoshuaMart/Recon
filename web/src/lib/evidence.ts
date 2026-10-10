/**
 * Turning an observation payload into rows a person can read. Flattening everything into one line
 * per top-level key gave a wall (six scripts on one line, a thousand-character favicon, the
 * fingerprinter's headers lost in `chain[]`), so the structure is kept. It is still walked, not
 * named field by field, so a probe can report something new without a change here.
 */

/** One rendered row. Depth is how far in the payload it sits. */
export interface Row {
	key: string;
	depth: number;
	/** A scalar, or the empty string on a row that only opens a block. */
	value: string;
	/** A data: image, rendered as one rather than printed. */
	image?: string;
	/** The whole value when `value` was cut, so the hover carries it. */
	title?: string;
}

/**
 * Keys that do not earn their place, per layer. A denylist, not an allowlist, for the pivot rules'
 * reason: a keep-list silently hides whatever a producer starts reporting.
 *
 * The tcp ones forced this: `scanned_ports` is the configured list, identical on every asset, and
 * `closed_ports`/`filtered_ports` are that list minus the open ones, a hundred numbers saying
 * nothing to a reader. `host` repeats the key and `state` the card's layer state. What survives is
 * `open_ports`, the finding.
 */
const hidden: Record<string, Set<string>> = {
	tcp: new Set(['closed_ports', 'filtered_ports', 'scanned_ports', 'host', 'state']),
	// On dns the host repeats the asset's own key.
	dns: new Set(['host', 'name']),
	// `url` was once hidden as being the asset. The service is now the asset, so the requested URL is
	// the one thing the key cannot say: which scheme answered and which base URL the chain starts on.
	http: new Set(['host'])
};

/** How long a scalar may be before it is cut, with the whole of it on hover. */
const maxValue = 300;

export function rows(layer: string, data: unknown): Row[] {
	const out: Row[] = [];
	walk(out, layer, data, 0, '');
	return out;
}

function walk(out: Row[], layer: string, value: unknown, depth: number, key: string): void {
	if (value === null || value === undefined || value === '') return;

	if (Array.isArray(value)) {
		// An array of scalars reads as a list on one row: `open_ports 443, 5000, 80`
		// is what somebody wants, not five rows of one number.
		if (value.every((item) => typeof item !== 'object' || item === null)) {
			out.push({ key, depth, value: value.map(scalar).join(', ') });
			return;
		}
		// An array of objects is one block per element. The index is shown because
		// `chain` is ordered and the order is the redirect chain .
		out.push({ key, depth, value: value.length === 1 ? '' : `${value.length} entries` });
		value.forEach((item, i) => {
			walk(out, layer, item, depth + 1, value.length === 1 ? '' : `#${i + 1}`);
		});
		return;
	}

	if (typeof value === 'object') {
		if (key) out.push({ key, depth, value: '' });
		const entries = Object.entries(value as Record<string, unknown>)
			.filter(([name]) => !hidden[layer]?.has(name))
			.sort(([a], [b]) => a.localeCompare(b));
		for (const [name, nested] of entries) {
			walk(out, layer, nested, key ? depth + 1 : depth, name);
		}
		return;
	}

	const text = scalar(value);
	// A data: image is shown, not printed: a favicon arrives as one, and a thousand characters of
	// base64 is the least readable thing a payload carries.
	if (text.startsWith('data:image/')) {
		out.push({ key, depth, value: '', image: text });
		return;
	}
	if (text.length > maxValue) {
		out.push({ key, depth, value: text.slice(0, maxValue) + '…', title: text });
		return;
	}
	out.push({ key, depth, value: text });
}

function scalar(value: unknown): string {
	return typeof value === 'string' ? value : JSON.stringify(value);
}

/** One block of a payload: an object, an array of objects, or all the scalars at once. */
export interface Section {
	/** The payload key, empty on the block that gathers the scalars. */
	key: string;
	label: string;
	rows: Row[];
	/** Set when this block repeats another one byte for byte (the asset view). */
	duplicateOf?: string;
}

/**
 * The same walk, cut into blocks. `rows` suits the card's fold; on the asset view it was forty
 * screens of undifferentiated key-value, nesting carried by indentation alone. A block is
 * structural, from the payload's shape and never a list of keys worth grouping, which keeps this a
 * denylist: a new producer field gets a block or a scalar line with no change here.
 */
export function sections(layer: string, data: unknown): Section[] {
	if (data === null || typeof data !== 'object' || Array.isArray(data)) {
		return [{ key: '', label: 'value', rows: rows(layer, data) }];
	}

	const entries = Object.entries(data as Record<string, unknown>)
		.filter(([name]) => !hidden[layer]?.has(name))
		.sort(([a], [b]) => a.localeCompare(b));

	const blocks: Section[] = [];
	const scalars: Row[] = [];
	for (const [name, value] of entries) {
		if (isBlock(value)) {
			blocks.push({ key: name, label: name, rows: rows(layer, value) });
		} else {
			walk(scalars, layer, value, 0, name);
		}
	}

	markDuplicates(blocks, data as Record<string, unknown>);
	if (scalars.length > 0) blocks.push({ key: '', label: 'top-level values', rows: scalars });
	return blocks;
}

/** How many top-level keys a layer carries, once the denylist has had its say. */
export function keyCount(layer: string, data: unknown): number {
	if (data === null || typeof data !== 'object' || Array.isArray(data)) return 0;
	return Object.keys(data as Record<string, unknown>).filter((name) => !hidden[layer]?.has(name)).length;
}

function isBlock(value: unknown): boolean {
	if (Array.isArray(value)) return value.some((item) => item !== null && typeof item === 'object');
	return value !== null && typeof value === 'object';
}

/**
 * The one repetition worth naming. `http-check` writes the answering hop's headers twice: in
 * `chain` and at the top level where the projection reads them. Neither is wrong, but printed
 * in full they are forty identical lines in the evidence. The block is kept and marked, not
 * dropped: a fold removing a key is one nobody can trust.
 */
function markDuplicates(blocks: Section[], data: Record<string, unknown>): void {
	const chain = data.chain;
	if (!Array.isArray(chain) || chain.length === 0) return;

	const answered = (chain[chain.length - 1] as Record<string, unknown> | null)?.headers;
	if (answered === undefined || data.headers === undefined) return;
	if (JSON.stringify(answered) !== JSON.stringify(data.headers)) return;

	for (const block of blocks) {
		if (block.key === 'headers') block.duplicateOf = `chain #${chain.length}`;
	}
}

/**
 * The favicon, out of the fingerprint payload. Free on the asset view, which already reads the
 * journal, so the bytes are in hand. The list is another question (the projection carries the
 * hash, not the image) and this deliberately does not work around that.
 */
export function faviconOf(evidence: { layer: string; data: unknown }[]): string | undefined {
	const render = evidence.find((layer) => layer.layer === 'fingerprint');
	if (!render) return undefined;
	const metadata = (render.data as { metadata?: { favicon?: unknown } })?.metadata;
	const favicon = metadata?.favicon;
	return typeof favicon === 'string' && favicon.startsWith('data:image/') ? favicon : undefined;
}
