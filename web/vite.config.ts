import adapter from '@sveltejs/adapter-static';
import { sveltekit } from '@sveltejs/kit/vite';
import { defineConfig } from 'vite';

// The API in development: a write with the session cookie must carry the
// origin of public_url, so the proxy presents it.
const api = 'http://localhost:8080';

export default defineConfig({
	plugins: [
		sveltekit({
			// A single page app: the Go binary answers index.html for every path it does not own.
			adapter: adapter({ pages: 'build', assets: 'build', fallback: 'index.html', strict: true })
		})
	],
	// Every asset a file of its own: the policy allows no data: fonts or scripts.
	build: { assetsInlineLimit: 0 },
	server: {
		proxy: {
			'/api': {
				target: api,
				headers: { origin: api }
			}
		}
	}
});
