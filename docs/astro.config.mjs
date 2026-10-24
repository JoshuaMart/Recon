// @ts-check
import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';
import mermaid from 'astro-mermaid';

// https://astro.build/config
export default defineConfig({
	integrations: [
		mermaid({ autoTheme: true }),
		starlight({
			title: 'Recon',
			logo: { src: './src/assets/logo.svg' },
			customCss: ['./src/styles/theme.css'],
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
