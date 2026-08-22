declare global {
	namespace App {
		interface Locals {
			/**
			 * The organization's api_token, read out of the httpOnly cookie by
			 * hooks.server.ts. It never reaches the browser and never reaches a
			 * load function's return value.
			 */
			token?: string;
		}
	}
}

export {};
