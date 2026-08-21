// @ts-check
import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';

// https://astro.build/config
export default defineConfig({
	integrations: [
		starlight({
			title: 'Recon',
			description: 'Design record of the Recon attack surface management platform.',
			defaultLocale: 'root',
			locales: {
				root: { label: 'English', lang: 'en' },
			},
			sidebar: [
				{
					label: 'Architecture',
					items: [{ autogenerate: { directory: 'architecture' } }],
				},
			],
		}),
	],
});
