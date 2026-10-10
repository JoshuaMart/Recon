/**
 * Where the connect screen may send someone afterwards. `next` is written by whoever built the link,
 * on a login screen the one place an attacker is invited to. A leading-slash check is not enough:
 * `//evil.example` starts with a slash and is an absolute address, and browsers normalise
 * `/\evil.example` to it, so the guard must know both forms. A separate module because its value is
 * the cases it refuses, worth a test.
 */
export function safeNext(next: string | null | undefined): string {
	if (!next || !next.startsWith('/')) return '/';
	if (next.startsWith('//') || next.startsWith('/\\')) return '/';
	return next;
}
