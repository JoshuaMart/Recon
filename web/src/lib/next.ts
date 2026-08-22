/**
 * Where the connect screen is allowed to send somebody afterwards.
 *
 * The `next` parameter is written by whoever built the link, which on a login screen is
 * the one place an attacker is invited to write one. A check on the leading slash is the
 * obvious guard and it is not enough: `//evil.example` starts with a slash and is an
 * **absolute** address, since a URL with no scheme keeps the current one. A browser
 * follows it off the console without a word, and the page it lands on is a login form
 * that looks exactly like the one just left.
 *
 * The backslash form is the same trap through a different parser: browsers normalise
 * `/\evil.example` to `//evil.example` before resolving it, so a guard that only knows
 * about the second slash is a guard somebody walks around.
 *
 * A separate module rather than a closure in the form action, because the value of this
 * function is entirely in the cases it refuses, and those are worth a test.
 */
export function safeNext(next: string | null | undefined): string {
	if (!next || !next.startsWith('/')) return '/';
	if (next.startsWith('//') || next.startsWith('/\\')) return '/';
	return next;
}
