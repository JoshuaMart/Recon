import { goto } from '$app/navigation';

/** An answer the API refused, with the body it gave. */
export class ApiError extends Error {
	constructor(
		public status: number,
		public body: { error?: string; detail?: string; suggest?: string; position?: number; field?: string }
	) {
		super(body.detail || body.error || `HTTP ${status}`);
	}
}

async function call<T>(method: string, path: string, body?: unknown): Promise<T> {
	const res = await fetch(path, {
		method,
		credentials: 'same-origin',
		headers: body === undefined ? {} : { 'Content-Type': 'application/json' },
		body: body === undefined ? undefined : JSON.stringify(body)
	});
	if (res.status === 401 && !path.startsWith('/api/session')) {
		const next = location.pathname + location.search;
		await goto('/login?next=' + encodeURIComponent(next));
		// The page is gone: settle nothing, so no caller reports an error.
		return new Promise<T>(() => {});
	}
	if (res.status === 204) return undefined as T;
	const data = await res.json().catch(() => ({}));
	if (!res.ok) throw new ApiError(res.status, data);
	return data as T;
}

export const api = {
	get: <T>(path: string) => call<T>('GET', path),
	post: <T>(path: string, body: unknown = {}) => call<T>('POST', path, body),
	patch: <T>(path: string, body: unknown) => call<T>('PATCH', path, body),
	del: <T>(path: string) => call<T>('DELETE', path)
};

/** A query string from the set values only. */
export function qs(params: Record<string, string | number | undefined | null>): string {
	const u = new URLSearchParams();
	for (const [k, v] of Object.entries(params)) if (v !== undefined && v !== null && v !== '') u.set(k, String(v));
	const s = u.toString();
	return s ? '?' + s : '';
}

export const shotURL = (key: string) => '/api/screenshots/' + key;
