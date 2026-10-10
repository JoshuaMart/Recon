import { dev } from '$app/environment';
import { COOKIE } from '../../hooks.server';
import { safeNext } from '$lib/next';
import { APIError, get } from '$lib/server/api';
import { fail, redirect, type Actions } from '@sveltejs/kit';

/**
 * The way in: one screen, one field, one cookie. Not a login: no password, session table or
 * `app_user` row. The credential is the organization's api_token and this only decides where it is
 * kept. The cost is written in the console chapter, not discovered later: the token belongs to the
 * organisation, so the console cannot say who acted.
 */
export const actions: Actions = {
	default: async ({ request, cookies, fetch, url }) => {
		const form = await request.formData();
		const token = String(form.get('token') ?? '').trim();
		if (!token) return fail(400, { message: 'Paste a token first.' });

		// The token is checked against the control plane before the cookie is set. Storing an unverified
		// one trades a message on this screen for a 403 on every page, the same failure from further away.
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
			// Lax is what makes a cookie usable in front of a POST API. The search endpoints are POST (the
			// query is a tree), and a session cookie in front of a POST API is a CSRF vector when unbounded.
			// Lax sends it on top-level GET navigations only.
			sameSite: 'lax',
			secure: !dev,
			maxAge: 60 * 60 * 24 * 30
		});

		// $lib/next, not a leading-slash check written here: `//evil.example` starts with one and is an
		// absolute address, so the guard must know more than the first character.
		redirect(303, safeNext(url.searchParams.get('next')));
	}
};
