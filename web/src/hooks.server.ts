import { dev } from '$app/environment';
import { env } from '$env/dynamic/private';
import { redirect, type Handle, type ServerInit } from '@sveltejs/kit';

/** The cookie that holds the organization's api_token. Named once. */
export const COOKIE = 'recon_token';

/**
 * ORIGIN is required in production, and the server refuses to start without it
 * rather than letting the failure appear at the first form.
 *
 * The node adapter compares the `Origin` header of a POST against the origin it
 * believes it serves, and without this variable it derives that badly: it then
 * refuses **legitimate** POSTs, the connect screen included. So the symptom is not
 * "CSRF protection missing", it is "nobody can log in", and it shows up at the
 * first submission rather than at boot.
 *
 * In `init` and not at module scope, which was the first attempt and was wrong:
 * SvelteKit imports this module during its own postbuild analysis, where `dev` is
 * false and no ORIGIN exists, so the check failed every `vite build` — including in
 * CI, where nothing is being served at all. `init` runs once when the server starts
 * responding, which is the moment the variable actually has to be there.
 *
 * Not enforced in development, where the origin check does not run at all: it
 * lives inside `if (!__SVELTEKIT_DEV__)` in SvelteKit's own request path, which is
 * also why a local test against `vite dev` demonstrates nothing about it.
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
 * How the credential is read, and where it stops.
 *
 * The token is pasted once and kept in an httpOnly cookie, so the browser never
 * sees it again and no script on the page can read it. That matters more here than
 * in most applications: this interface renders titles and headers collected from
 * hostile targets, which is the last place to bet that no XSS will ever land.
 *
 * The token goes onto `locals` and never into anything a load function returns.
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
