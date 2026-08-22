import { COOKIE } from '../../hooks.server';
import { redirect, type RequestHandler } from '@sveltejs/kit';

/**
 * Drops the cookie. POST rather than GET, so a link in an email or a prefetch
 * cannot end somebody's session.
 *
 * This ends the browser's hold on the token and nothing else. Revoking the token
 * itself is a row in `api_token`, which is what actually takes the credential
 * away from whoever also has a copy of it.
 */
export const POST: RequestHandler = ({ cookies }) => {
	cookies.delete(COOKIE, { path: '/' });
	redirect(303, '/connect');
};
