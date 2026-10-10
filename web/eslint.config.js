import js from '@eslint/js';
import svelte from 'eslint-plugin-svelte';
import globals from 'globals';
import ts from 'typescript-eslint';

export default ts.config(
	js.configs.recommended,
	...ts.configs.recommended,
	...svelte.configs.recommended,
	{
		languageOptions: {
			globals: { ...globals.browser, ...globals.node }
		}
	},
	{
		files: ['**/*.svelte', '**/*.svelte.ts', '**/*.svelte.js'],
		languageOptions: {
			parserOptions: {
				projectService: true,
				extraFileExtensions: ['.svelte'],
				// svelte-eslint-parser handles the markup and hands the script block to this one. Without it every
				// `<script lang="ts">` fails to parse, and an unparsed file is one nothing is checked in.
				parser: ts.parser
			}
		}
	},
	{
		rules: {
			// Off deliberately. The rule wants every internal href through `resolve()`, guarding a base path the
			// console lacks. Every link is built by $lib/query, the one place that constructs a URL and where a
			// base path would be applied if one appears.
			'svelte/no-navigation-without-resolve': 'off'
		}
	},
	{ ignores: ['.svelte-kit/', 'build/'] }
);
