import { dev } from '$app/environment';
import { env } from '$env/dynamic/private';
import { redirect, type Handle, type ServerInit } from '@sveltejs/kit';

/** The cookie that holds the organization's api_token. Named once. */
export const COOKIE = 'recon_token';

/**
 * ORIGIN is required in production; the server refuses to start without it rather than failing
 * at the first form. The node adapter compares a POST's `Origin` header against the origin it
 * believes it serves and derives it badly without this, refusing legitimate POSTs (the connect
 * screen included): the symptom is "nobody can log in", not "CSRF protection missing".
 *
 * In `init`, not module scope: SvelteKit imports this module during postbuild analysis, where no
 * ORIGIN exists, which failed every `vite build`. Not enforced in dev, where the origin check does
 * not run, so a local test against `vite dev` demonstrates nothing about it.
 */
export const init: ServerInit = () => {
	if (!dev && !env.ORIGIN) {
		throw new Error(
			'ORIGIN is required in production. Without it the node adapter rejects every ' +
				'form submission, including the connect screen. Set it to the URL the console ' +
				'is served on, for example https://recon.example.com'
		);
	}
};

/**
 * How the credential is read, and where it stops. The token is pasted once and kept in an httpOnly
 * cookie, so no page script can read it: this interface renders titles and headers collected from
 * hostile targets. It goes onto `locals`, never into anything a load function returns.
 */
export const handle: Handle = async ({ event, resolve }) => {
	const token = event.cookies.get(COOKIE);
	if (token) event.locals.token = token;

	const open = event.url.pathname === '/connect';
	if (!token && !open) {
		redirect(303, '/connect?next=' + encodeURIComponent(event.url.pathname + event.url.search));
	}
	if (token && open) redirect(303, '/');

	return resolve(event);
};
