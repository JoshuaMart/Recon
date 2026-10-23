/** The top-level terms of a query, split on spaces outside quotes and
 * parentheses. A query with groups or OR is shown as one piece. */
export function terms(q: string): string[] {
	const out: string[] = [];
	let cur = '';
	let quote = false;
	let depth = 0;
	for (const ch of q) {
		if (ch === '"') quote = !quote;
		else if (!quote && ch === '(') depth++;
		else if (!quote && ch === ')') depth = Math.max(0, depth - 1);
		if (!quote && depth === 0 && /\s/.test(ch)) {
			if (cur) out.push(cur);
			cur = '';
			continue;
		}
		cur += ch;
	}
	if (cur) out.push(cur);
	return out;
}

/** The query with term added, or removed when it is already there. */
export function toggle(q: string, term: string): string {
	const ts = terms(q);
	const i = ts.indexOf(term);
	if (i >= 0) ts.splice(i, 1);
	else ts.push(term);
	return ts.join(' ');
}

export function without(q: string, index: number): string {
	const ts = terms(q);
	ts.splice(index, 1);
	return ts.join(' ');
}
