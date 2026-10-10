import { env } from '$env/dynamic/private';
import { error } from '@sveltejs/kit';

/**
 * The proxy. The browser carries no credential, so every control-plane call is made from here
 * with the api_token read from the cookie. That is why this console has a server, and why there
 * is no client-side fetch of the API anywhere.
 */

/** Where the control plane listens. One place reads the environment. */
export function apiURL(): string {
	return env.RECON_API_URL ?? 'http://localhost:8080';
}

export class APIError extends Error {
	constructor(
		readonly status: number,
		message: string,
		/** The machine readable half, so a caller can branch on it where the
		 *  sentence is only for a person. A stale version is not a bad request. */
		readonly reason = ''
	) {
		super(message);
	}
}

/**
 * call posts a JSON body and decodes a JSON answer. A 401 becomes a typed error, not a redirect:
 * the caller knows whether it checks a token just typed or serves a page whose cookie was
 * revoked, which want different answers.
 */
export async function call<T>(token: string, path: string, body: unknown, fetcher: typeof fetch = fetch): Promise<T> {
	return send<T>('POST', token, path, body, fetcher);
}

/** patch is `call` with the other method, for the writes that carry a version. */
export async function patch<T>(token: string, path: string, body: unknown, fetcher: typeof fetch = fetch): Promise<T> {
	return send<T>('PATCH', token, path, body, fetcher);
}

async function send<T>(method: string, token: string, path: string, body: unknown, fetcher: typeof fetch): Promise<T> {
	const response = await fetcher(apiURL() + path, {
		method,
		headers: {
			authorization: 'Bearer ' + token,
			'content-type': 'application/json'
		},
		body: JSON.stringify(body ?? {})
	});

	if (!response.ok) throw await refusal(response);
	return (await response.json()) as T;
}

/** get is the same thing for the read endpoints that take no body. */
export async function get<T>(token: string, path: string, fetcher: typeof fetch = fetch): Promise<T> {
	const response = await fetcher(apiURL() + path, {
		headers: { authorization: 'Bearer ' + token }
	});
	if (!response.ok) throw await refusal(response);
	return (await response.json()) as T;
}

/**
 * stream forwards a response body without buffering it: the export must not materialize the
 * inventory before sending, and a buffering proxy would cancel that. The status line goes out before
 * the first row is read. The live feed does not come through here (a GET whose headers matter to the
 * browser); it forwards in its own route.
 */
export async function stream(
	token: string,
	path: string,
	body: unknown,
	fetcher: typeof fetch = fetch
): Promise<Response> {
	const upstream = await fetcher(apiURL() + path, {
		method: 'POST',
		headers: {
			authorization: 'Bearer ' + token,
			'content-type': 'application/json'
		},
		body: JSON.stringify(body ?? {})
	});

	if (!upstream.ok) throw await refusal(upstream);

	const headers = new Headers();
	for (const name of ['content-type', 'content-disposition', 'cache-control']) {
		const value = upstream.headers.get(name);
		if (value) headers.set(name, value);
	}
	return new Response(upstream.body, { status: 200, headers });
}

/**
 * refusal pulls the control plane's own error out of the body, both halves. A refused filter
 * carries the clause it choked on, so the console can point at the facet just clicked and not the
 * request. And the machine-readable reason separates a stale version from a malformed body, which
 * look alike to a status code.
 */
async function refusal(response: Response): Promise<APIError> {
	try {
		const body = (await response.json()) as { error?: string; detail?: string };
		if (body.detail || body.error) {
			return new APIError(response.status, body.detail ?? body.error ?? '', body.error ?? '');
		}
	} catch {
		// A body that is not JSON says nothing useful. Fall through.
	}
	return new APIError(response.status, 'the control plane answered ' + response.status);
}

/** fail turns an APIError into the page a visitor should see. */
export function fail(err: unknown): never {
	if (err instanceof APIError) {
		if (err.status === 400) error(400, err.message);
		if (err.status === 404) error(404, err.message);
		if (err.status === 401 || err.status === 403) error(403, 'this token cannot read the inventory');
		error(502, err.message);
	}
	throw err;
}
