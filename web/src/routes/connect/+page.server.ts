import { dev } from '$app/environment';
import { COOKIE } from '../../hooks.server';
import { safeNext } from '$lib/next';
import { APIError, get } from '$lib/server/api';
import { fail, redirect, type Actions } from '@sveltejs/kit';

/**
 * The way in. One screen, one field, one cookie.
 *
 * This is not a login: no password, no session table, no row of `app_user`. The
 * credential is the organization's api_token, and
 * phase 7 only decides where it is kept. What that costs is written down in
 * the console chapter rather than discovered later: the token belongs to the organisation, so
 * the console cannot say who acted.
 */
export const actions: Actions = {
	default: async ({ request, cookies, fetch, url }) => {
		const form = await request.formData();
		const token = String(form.get('token') ?? '').trim();
		if (!token) return fail(400, { message: 'Paste a token first.' });

		// The token is checked against the control plane before the cookie is set.
		// Storing an unverified one would trade a message on this screen for a 403
		// on every page, which is the same failure reported from further away.
		try {
			await get<{ fields: string[] }>(token, '/assets/fields', fetch);
		} catch (err) {
			if (err instanceof APIError && (err.status === 401 || err.status === 403)) {
				return fail(401, { message: 'The control plane refused this token.' });
			}
			return fail(502, {
				message: 'The control plane could not be reached. ' + (err instanceof Error ? err.message : '')
			});
		}

		cookies.set(COOKIE, token, {
			path: '/',
			httpOnly: true,
			// Lax is what makes a cookie usable in front of a POST API. The search
			// endpoints are POST because the query is a tree , and a session
			// cookie in front of a POST API is a CSRF vector when nothing bounds it.
			// Lax sends the cookie on top-level GET navigations only.
			sameSite: 'lax',
			secure: !dev,
			maxAge: 60 * 60 * 24 * 30
		});

		// $lib/next, and not a check on the leading slash written here: `//evil.example`
		// starts with one and is an absolute address, so the guard has to know more than
		// the first character.
		redirect(303, safeNext(url.searchParams.get('next')));
	}
};
