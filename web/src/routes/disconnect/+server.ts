import { COOKIE } from '../../hooks.server';
import { redirect, type RequestHandler } from '@sveltejs/kit';

/**
 * Drops the cookie. POST, not GET, so a link in an email or a prefetch cannot end someone's session.
 * This ends the browser's hold on the token and nothing else: revoking the token itself is a row in
 * `api_token`, which actually takes the credential from whoever also has a copy.
 */
export const POST: RequestHandler = ({ cookies }) => {
	cookies.delete(COOKIE, { path: '/' });
	redirect(303, '/connect');
};
