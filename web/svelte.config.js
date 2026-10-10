import adapter from '@sveltejs/adapter-node';
import { vitePreprocess } from '@sveltejs/vite-plugin-svelte';

/**
 * The node adapter, not a static one, on purpose: the console holds the organisation's api_token in an
 * httpOnly cookie and adds the Authorization header itself, so it needs a server; a static bundle has
 * nowhere to keep a secret.
 *
 * @type {import('@sveltejs/kit').Config}
 */
export default {
	preprocess: vitePreprocess(),
	kit: {
		adapter: adapter()
		// `csrf.trustedOrigins` is deliberately unset, leaving the origin check refusing every cross-origin
		// form POST. Every mutating route is a form POST carrying the session cookie, and SameSite=Lax alone
		// would not cover a same-site subdomain. Adding an origin would make a cookie-backed proxy in front of
		// a POST API a CSRF vector, so it stays empty until something needs it.
	}
};
