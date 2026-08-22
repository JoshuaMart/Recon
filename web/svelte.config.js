import adapter from '@sveltejs/adapter-node';
import { vitePreprocess } from '@sveltejs/vite-plugin-svelte';

/**
 * The node adapter and not a static one, on purpose. 13.9bis: the console holds
 * the organisation's api_token in an httpOnly cookie and adds the Authorization
 * header itself, so it needs a server. A static bundle has nowhere to keep a
 * secret, and that is what decided the framework rather than any preference.
 *
 * @type {import('@sveltejs/kit').Config}
 */
export default {
	preprocess: vitePreprocess(),
	kit: {
		adapter: adapter()
		// `csrf.trustedOrigins` is deliberately not set, which leaves the origin
		// check at its default of refusing every cross-origin form POST. Every
		// mutating route here is a form POST carrying the session cookie, and
		// SameSite=Lax alone would not cover a same-site subdomain. Adding an
		// origin to that list is what would make a cookie-backed proxy in front of
		// a POST API a CSRF vector, so it stays empty until something needs it.
	}
};
