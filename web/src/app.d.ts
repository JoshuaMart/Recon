declare global {
	namespace App {
		interface Locals {
			/**
			 * The organization's api_token, read from the httpOnly cookie by hooks.server.ts. It never reaches
			 * the browser or a load function's return value.
			 */
			token?: string;
		}
	}
}

export {};
