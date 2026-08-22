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
				// svelte-eslint-parser handles the markup and hands the script block
				// to this one. Without it every `<script lang="ts">` fails to parse,
				// and a file that fails to parse is a file nothing is checked in.
				parser: ts.parser
			}
		}
	},
	{
		rules: {
			// Off deliberately. The rule wants every internal href to go through
			// `resolve()`, which guards against a base path the console does not
			// have. Every link here is built by $lib/query, which is the one place
			// that constructs a URL, and that is where a base path would be applied
			// if one ever appears.
			'svelte/no-navigation-without-resolve': 'off'
		}
	},
	{ ignores: ['.svelte-kit/', 'build/'] }
);
