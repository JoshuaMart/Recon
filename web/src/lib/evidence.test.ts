import { describe, expect, it } from 'vitest';
import { keyCount, rows, sections } from './evidence';

/**
 * The cases are the ones from the real inventory that made this module necessary,
 * because the version it replaces flattened all three into one line each.
 */
describe('rows', () => {
	it('keeps the fingerprinter headers, which live inside chain[]', () => {
		// They were invisible before: `chain` is an array of objects and joining it
		// put the headers inside a blob with the status code and the title.
		const out = rows('fingerprint', {
			chain: [
				{
					url: 'https://qui.jomar.ovh/',
					status_code: 200,
					headers: { via: '1.1 Caddy', 'content-type': 'text/html; charset=utf-8' }
				}
			]
		});
		const keys = out.map((row) => row.key);
		expect(keys).toContain('headers');
		expect(keys).toContain('via');
		const via = out.find((row) => row.key === 'via');
		expect(via?.value).toBe('1.1 Caddy');
		// And the nesting is carried, so a header reads as being inside the hop.
		expect(via!.depth).toBeGreaterThan(out.find((row) => row.key === 'chain')!.depth);
	});

	// the identity rule moved the identity to the service, so the requested URL stopped being a
	// repeat of the asset's own key and became the one thing that key cannot say:
	// which scheme answered. It was on the http denylist for the old reason.
	it('shows the requested url on the http layer', () => {
		const out = rows('http', {
			url: 'https://qui.jomar.ovh',
			host: 'qui.jomar.ovh',
			status_code: 200
		});
		const keys = out.map((row) => row.key);
		expect(keys).toContain('url');
		// And `host` stays hidden, because that one does repeat the key.
		expect(keys).not.toContain('host');
	});

	it('renders a data: favicon as an image instead of printing it', () => {
		const uri = 'data:image/png;base64,iVBORw0KGgoAAAANSUhEUg';
		const out = rows('fingerprint', { metadata: { favicon: uri, robots_txt: true } });
		const favicon = out.find((row) => row.key === 'favicon');
		expect(favicon?.image).toBe(uri);
		expect(favicon?.value).toBe('');
		// The flags beside it stay readable rows of their own rather than being
		// appended to a thousand characters of base64.
		expect(out.find((row) => row.key === 'robots_txt')?.value).toBe('true');
	});

	it('gives each script its own block', () => {
		const out = rows('fingerprint', {
			scripts: [
				{ url: 'https://a.test/one.js', hash: 'aaa', internal: true },
				{ url: 'https://a.test/two.js', hash: 'bbb', internal: true }
			]
		});
		expect(out.filter((row) => row.key === 'hash')).toHaveLength(2);
		// Numbered, because an ordered array's order is information: `chain` is the
		// redirect chain.
		expect(out.map((row) => row.key)).toContain('#1');
	});

	it('drops the tcp keys that do not earn their place', () => {
		const out = rows('tcp', {
			open_ports: [443, 80],
			closed_ports: [1, 2, 3],
			scanned_ports: [1, 2, 3, 80, 443],
			host: 'qui.jomar.ovh',
			state: 'open'
		});
		const keys = out.map((row) => row.key);
		expect(keys).toEqual(['open_ports']);
		// A list of scalars stays on one row: that is what somebody wants to read.
		expect(out[0].value).toBe('443, 80');
	});

	it('cuts a long scalar and keeps the whole of it for the hover', () => {
		const long = 'x'.repeat(500);
		const [row] = rows('http', { raw_body_hash: long });
		expect(row.value.endsWith('…')).toBe(true);
		expect(row.title).toBe(long);
	});

	it('drops nothing on a layer with no list of its own', () => {
		// The denylist is per layer, and a layer absent from it hides nothing. A
		// missing entry must not silently hide a field.
		const out = rows('fingerprint', { host: 'qui.jomar.ovh' });
		expect(out.map((row) => row.key)).toEqual(['host']);
	});
});

/**
 * The blocks of the raw fold. The cases are the ones that made the flat run unreadable on the
 * asset view: a payload whose nesting was carried by indentation alone, and one key
 * printed twice in full.
 */
describe('sections', () => {
	it('gives every object and array its own block, and gathers the scalars into one', () => {
		const out = sections('http', {
			chain: [{ code: 301, url: 'http://a.example' }],
			tls: { version: 'TLS 1.3' },
			status_code: 200,
			title: 'Lighthouse'
		});
		const labels = out.map((section) => section.label);
		expect(labels).toContain('chain');
		expect(labels).toContain('tls');
		// The scalars go last, together, rather than one block each.
		expect(labels.at(-1)).toBe('top-level values');
		const values = out.at(-1)!.rows.map((row) => row.key);
		expect(values).toEqual(['status_code', 'title']);
	});

	// `http-check` writes the answering hop's headers twice: inside `chain`, and again at
	// the top level where the projection reads them. Printed in full one after the other
	// they are forty identical lines in the middle of the evidence.
	it('marks a headers block that repeats the answering hop', () => {
		const headers = { 'content-type': 'text/html', server: 'Caddy' };
		const out = sections('http', {
			chain: [
				{ code: 301, headers: { location: 'https://a.example/' } },
				{ code: 200, headers }
			],
			headers
		});
		expect(out.find((section) => section.key === 'headers')?.duplicateOf).toBe('chain #2');
	});

	// The discriminating case: one header differs, so the block is not a repeat and
	// hiding it would hide a value nothing else carries.
	it('leaves a headers block alone when it differs from the hop', () => {
		const out = sections('http', {
			chain: [{ code: 200, headers: { server: 'Caddy' } }],
			headers: { server: 'nginx' }
		});
		expect(out.find((section) => section.key === 'headers')?.duplicateOf).toBeUndefined();
	});

	it('keeps applying the denylist, so the tcp settings stay out', () => {
		const out = sections('tcp', {
			open_ports: [80],
			scanned_ports: [80, 443],
			closed_ports: [443],
			host: 'a.example',
			state: 'open'
		});
		const keys = out.flatMap((section) => section.rows.map((row) => row.key));
		expect(keys).toContain('open_ports');
		expect(keys).not.toContain('scanned_ports');
		expect(keys).not.toContain('host');
	});
});

describe('keyCount', () => {
	it('counts the top-level keys the fold will show, and not the ones it hides', () => {
		expect(keyCount('tcp', { open_ports: [80], scanned_ports: [80], host: 'a.example' })).toBe(1);
		expect(keyCount('http', { url: 'a', status_code: 200 })).toBe(2);
		expect(keyCount('http', null)).toBe(0);
	});
});
