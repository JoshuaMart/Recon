const units: [number, string][] = [
	[60, 'second'],
	[60, 'minute'],
	[24, 'hour'],
	[30, 'day'],
	[12, 'month'],
	[Infinity, 'year']
];

/** "3 days ago", "in 2 hours". */
export function ago(iso: string | null | undefined, now = Date.now()): string {
	if (!iso) return 'never';
	let d = (now - new Date(iso).getTime()) / 1000;
	const future = d < 0;
	d = Math.abs(d);
	if (d < 45) return future ? 'in a moment' : 'just now';
	for (const [n, unit] of units) {
		if (d < n) {
			const v = Math.round(d);
			const s = `${v} ${unit}${v === 1 ? '' : 's'}`;
			return future ? `in ${s}` : `${s} ago`;
		}
		d /= n;
	}
	return iso;
}

/** Whole days since, null when never. */
export function daysSince(iso: string | null | undefined, now = Date.now()): number | null {
	return iso ? Math.floor((now - new Date(iso).getTime()) / 86_400_000) : null;
}

export function dateTime(iso: string | null | undefined): string {
	if (!iso) return '—';
	return new Date(iso).toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' });
}

const nf = new Intl.NumberFormat('en-US');
export const count = (n: number) => nf.format(n).replace(/,/g, ' ');

export function bytes(n: number): string {
	const u = ['B', 'KB', 'MB', 'GB', 'TB'];
	let i = 0;
	while (n >= 1024 && i < u.length - 1) {
		n /= 1024;
		i++;
	}
	return `${n < 10 && i > 0 ? n.toFixed(1) : Math.round(n)} ${u[i]}`;
}

export function duration(from: string | null, to: string | null): string {
	if (!from) return '—';
	const s = Math.round(((to ? new Date(to).getTime() : Date.now()) - new Date(from).getTime()) / 1000);
	if (s < 60) return `${s}s`;
	if (s < 3600) return `${Math.floor(s / 60)}m ${s % 60}s`;
	return `${Math.floor(s / 3600)}h ${Math.floor((s % 3600) / 60)}m`;
}

/** The badge tone of an HTTP status. */
export function statusTone(code: number | null): string {
	if (code === null) return '';
	if (code < 300) return 'ok';
	if (code < 400) return 'info';
	if (code < 500) return 'warn';
	return 'bad';
}

/** "nginx@1.22.1" as name and version. */
export function splitTech(t: string): { name: string; version: string } {
	const i = t.lastIndexOf('@');
	return i > 0 ? { name: t.slice(0, i), version: t.slice(i + 1) } : { name: t, version: '' };
}

export const label = (s: string) => s.replace(/_/g, ' ');

/** An asset key as shown: a TCP service without its protocol suffix. */
export const displayKey = (kind: string, key: string) => (kind === 'service' ? key.replace(/\/tcp$/, '') : key);

/** A due date: "due now" once it has passed, which a probe asked by hand always has. */
export const due = (iso: string | null | undefined) =>
	!iso ? '—' : new Date(iso).getTime() <= Date.now() ? 'due now' : ago(iso);
