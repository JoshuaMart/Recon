<script lang="ts">
	import { ago, exact } from '$lib/format';
	import type { Change, DiffField } from '$lib/types';

	interface Props {
		timeline: Change[];
		truncated: string[];
	}

	const { timeline, truncated }: Props = $props();

	/**
	 * What a class means in words.
	 *
	 * The whole point of the classification is that a revelation is not an alert: the
	 * instrument came to see better and the target did not move. Rendering the two the
	 * same way would put the distinction back in the reader's head, which is where it
	 * was before the producer version was stored twice.
	 *
	 * The class sits on the transition and not on each field, because that is what it
	 * describes: a pure addition across a version bump is the observer moving, and one
	 * field of the same diff cannot be a revelation while another is not.
	 */
	const classes: Record<string, { label: string; tone: string }> = {
		real_change: { label: 'changed', tone: 'real' },
		detection_improved: { label: 'better detection', tone: 'reveal' }
	};

	/**
	 * A changed value, rendered.
	 *
	 * The payloads are the producer's, so a side of a diff is a list, a scalar or an
	 * object depending on the field, and typing it as a list of strings is what put
	 * `[object Object]` on the screen. Anything that is not a string or a list of them
	 * is written as JSON, which is at least true.
	 */
	function values(value: unknown, limit = 4): string {
		if (value === undefined || value === null) return '';
		if (Array.isArray(value)) {
			const list = value.map((item) => (typeof item === 'string' ? item : JSON.stringify(item)));
			if (list.length === 0) return '';
			if (list.length <= limit) return list.join(', ');
			return list.slice(0, limit).join(', ') + ' and ' + (list.length - limit) + ' more';
		}
		return typeof value === 'string' ? value : JSON.stringify(value);
	}

	/** How many members a side of a diff carries, for the choice below. */
	function size(value: unknown): number {
		if (value === undefined || value === null) return 0;
		return Array.isArray(value) ? value.length : 1;
	}

	/**
	 * Which of the two forms a changed field takes, because the server sends both.
	 *
	 * `internal/diff` carries `before`/`after` — the whole field — and `added`/`removed`
	 * — the delta — on purpose, and says why in the same breath: "+ PHPSESSID" is
	 * unreadable without knowing there were nine cookies before, and the full pair is
	 * unreadable when there are ninety. Choosing between them is therefore the screen's
	 * job, and rendering both is what produced `open_ports changed → 443 + 443`, the
	 * same value written twice.
	 *
	 * The pair when there is something to compare and it is short enough to read; the
	 * delta otherwise, including when one side is empty — a field that appeared reads as
	 * "+ 443" and never as "→ 443".
	 */
	const pairLimit = 6;

	function form(field: DiffField): 'pair' | 'delta' {
		const before = size(field.before);
		const after = size(field.after);
		const delta = (field.added?.length ?? 0) + (field.removed?.length ?? 0);
		if (delta === 0) return 'pair';
		if (before === 0 || after === 0) return 'delta';
		return before + after <= pairLimit ? 'pair' : 'delta';
	}

	/**
	 * A first observation is not a change, and four of them are not four changes.
	 *
	 * On a fresh inventory every layer has exactly one state, so the panel was four
	 * entries each apologising in the same words. They are collapsed into one line naming
	 * the layers, and the timeline shows what it exists to show: the changes.
	 */
	const changes = $derived(timeline.filter((entry) => entry.diff));
	const firsts = $derived(timeline.filter((entry) => !entry.diff));
	const firstLayers = $derived([...new Set(firsts.map((entry) => entry.layer))]);

	/**
	 * The layer filter, over what is already loaded.
	 *
	 * No request behind it: the asset view answers all three parts in one response, so narrowing to
	 * one layer is a filter on an array. A chip and not a select, because there are four
	 * of them at most and the count is worth seeing.
	 */
	let only = $state<Change['layer'] | ''>('');
	const layers = $derived([...new Set(changes.map((entry) => entry.layer))]);
	// The filter is only honoured while the layer it names is still on screen.
	// State survives a navigation between two assets on the same route, and the
	// tab row only renders above one layer, so a filter left on `http` followed
	// by an asset whose changes are all `dns` showed an empty panel with no
	// control left to clear it. Self healing here rather than through an effect
	// that resets it, because the condition is the same one the tabs render on.
	const active = $derived(only && layers.includes(only) ? only : '');
	const shown = $derived(active ? changes.filter((entry) => entry.layer === active) : changes);

	/** The days, so a run of changes on one afternoon reads as one afternoon. */
	const days = $derived.by(() => {
		const out: { label: string; entries: Change[] }[] = [];
		for (const entry of shown) {
			const label = dayOf(entry.at);
			const last = out.at(-1);
			if (last && last.label === label) last.entries.push(entry);
			else out.push({ label, entries: [entry] });
		}
		return out;
	});

	function dayOf(when: string): string {
		const parsed = new Date(when);
		if (Number.isNaN(parsed.getTime())) return 'unknown date';
		const today = new Date();
		if (parsed.toDateString() === today.toDateString()) return 'Today';
		return parsed.toLocaleDateString('en-GB', { day: 'numeric', month: 'short', year: 'numeric' });
	}

	/** How long a state held, or that it is still holding. */
	function held(entry: Change): string {
		if (!entry.held_until || entry.at === entry.held_until) return 'seen once';
		const from = Date.parse(entry.at);
		const to = Date.parse(entry.held_until);
		if (Number.isNaN(from) || Number.isNaN(to)) return '';
		if (Date.now() - to < 3600_000) return 'still holding';
		return 'held ' + span(to - from);
	}

	function span(ms: number): string {
		const hours = Math.round(ms / 3600_000);
		if (hours < 24) return `${Math.max(1, hours)} h`;
		const days = Math.floor(hours / 24);
		const rest = hours % 24;
		return rest === 0 ? `${days} d` : `${days} d ${rest} h`;
	}
</script>

<section class="dv-panel">
	<header class="dv-head">
		<h2>Timeline</h2>
		<span class="dv-src">{changes.length} {changes.length === 1 ? 'change' : 'changes'} in the window</span>
		<span class="spacer"></span>
		{#if layers.length > 1}
			<button class="tab" class:on={active === ''} type="button" onclick={() => (only = '')}>all layers</button>
			{#each layers as layer (layer)}
				<button class="tab" class:on={active === layer} type="button" onclick={() => (only = layer)}>{layer}</button>
			{/each}
		{/if}
	</header>

	<div class="dv-body">
		{#if timeline.length === 0}
			<p class="dv-note">No observation in the window. Nothing has been measured here yet.</p>
		{/if}

		{#each days as day (day.label)}
			<div class="day">
				<span class="d">{day.label}</span>
				<span class="l"></span>
			</div>

			{#each day.entries as entry, i (entry.layer + entry.at + i)}
				<article class="entry" class:first={entry === changes[0]}>
					<div class="when">
						<span class="t" title={exact(entry.at)}>{ago(entry.at)}</span>
						<span class="h" title="held until {exact(entry.held_until)}">{held(entry)}</span>
					</div>

					<div class="track">
						<div class="head">
							<span class="dv-layer">{entry.layer}</span>
							{#if entry.outcome !== 'ok'}<span class="dv-outcome">{entry.outcome}</span>{/if}
						</div>

						{#if entry.diff?.fields?.length}
							<ul class="fields">
								{#each entry.diff.fields as field (field.field)}
									<li class="f {classes[entry.diff.class]?.tone ?? 'real'}">
										<span class="name">{field.field}</span>
										<span class="klass">{classes[entry.diff.class]?.label ?? entry.diff.class}</span>
										{#if form(field) === 'pair'}
											<span class="delta">
												{values(field.before)}
												<span class="arrow">→</span>
												{values(field.after)}
											</span>
										{:else}
											{#if field.added?.length}<span class="delta added">+ {values(field.added)}</span>{/if}
											{#if field.removed?.length}<span class="delta removed">− {values(field.removed)}</span>{/if}
										{/if}
									</li>
								{/each}
							</ul>
							{#if entry.diff.previous_producer_version && entry.producer_version !== entry.diff.previous_producer_version}
								<p class="inst">
									Instrument moved from {entry.diff.previous_producer_version} to {entry.producer_version ??
										'an unnamed version'}.
								</p>
							{/if}
						{:else}
							<p class="dv-note">A new state, with nothing this system compares having moved.</p>
						{/if}
					</div>
				</article>
			{/each}
		{/each}

		{#if shown.length === 0 && changes.length > 0}
			<p class="dv-note">No change on this layer in the window read.</p>
		{/if}

		{#if firsts.length}
			<!-- An absent diff means "not compared", which is not "unchanged". Said
			     once, naming the layers, rather than once per layer in identical words. -->
			<p class="first">
				First observation of {firstLayers.join(', ')}
				{#if changes.length === 0}— nothing has changed since, so there is nothing to compare.{:else}
					within the window read.{/if}
			</p>
		{/if}

		{#if truncated.length}
			<p class="cut">History cut for {truncated.join(', ')}. There are older states than the ones shown.</p>
		{/if}
	</div>

	{#if changes.length > 0}
		<footer class="legend">
			<span><i class="key real"></i>the target moved</span>
			<span><i class="key reveal"></i>the producer sees better</span>
			<span><i class="key lost"></i>the producer sees less</span>
			<span class="spacer"></span>
			<span class="dim">The same comparison the notification used, from the same function.</span>
		</footer>
	{/if}
</section>

<style>
	.tab {
		font-size: 11.5px;
		color: var(--ink-3);
		background: none;
		border: 0;
		padding: 2px 8px;
		border-radius: 999px;
	}

	.tab:hover {
		color: var(--ink);
	}

	.tab.on {
		background: var(--signal-bg);
		color: var(--ink);
		font-weight: 500;
	}

	.day {
		display: flex;
		align-items: center;
		gap: 10px;
		margin: 0 0 9px;
	}

	.day .d {
		font-size: 10.5px;
		text-transform: uppercase;
		letter-spacing: 0.06em;
		color: var(--ink-3);
		font-weight: 600;
	}

	.day .l {
		flex: 1;
		height: 1px;
		background: var(--border-2);
	}

	.entry + .day {
		margin-top: 4px;
	}

	.entry {
		display: grid;
		grid-template-columns: 118px minmax(0, 1fr);
		gap: 14px;
		padding-bottom: 14px;
	}

	.when {
		text-align: right;
		padding-top: 1px;
	}

	.when .t {
		font-size: 12px;
		color: var(--ink);
		font-weight: 500;
	}

	.when .h {
		display: block;
		font-size: 10.5px;
		color: var(--ink-3);
		margin-top: 1px;
	}

	.track {
		position: relative;
		padding-left: 20px;
		border-left: 1px solid var(--border);
		min-width: 0;
	}

	.track::before {
		content: '';
		position: absolute;
		left: -4.5px;
		top: 4px;
		width: 8px;
		height: 8px;
		border-radius: 50%;
		background: var(--card);
		border: 2px solid var(--border);
	}

	.entry.first .track::before {
		border-color: var(--signal);
	}

	.head {
		display: flex;
		align-items: center;
		gap: 8px;
		margin-bottom: 6px;
	}

	.fields {
		list-style: none;
		margin: 0;
		padding: 0;
		display: grid;
		gap: 3px;
	}

	.f {
		display: flex;
		align-items: baseline;
		gap: 8px;
		flex-wrap: wrap;
		font-size: 12px;
		padding: 4px 9px;
		border-radius: var(--radius-control);
		border-left: 2px solid var(--border);
		background: var(--canvas);
		min-width: 0;
	}

	.f.real {
		border-left-color: var(--signal);
	}

	.f.reveal {
		border-left-color: var(--code-3xx);
	}

	.f.lost {
		border-left-color: var(--code-4xx);
	}

	.name {
		font-family: var(--font-mono);
		font-size: 11.5px;
		color: var(--ink);
	}

	.klass {
		font-size: 9.5px;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: var(--ink-3);
	}

	.delta {
		font-family: var(--font-mono);
		font-size: 11px;
		overflow-wrap: anywhere;
		min-width: 0;
	}

	.delta.added {
		color: var(--code-2xx);
	}

	.delta.removed {
		color: var(--code-5xx);
	}

	.arrow {
		color: var(--ink-3);
	}

	.inst {
		margin: 6px 0 0;
		font-size: 11px;
		color: var(--ink-3);
	}

	.first {
		margin: 0;
		font-size: 12px;
		color: var(--ink-3);
		background: var(--canvas);
		border-radius: var(--radius-control);
		padding: 7px 10px;
	}

	.cut {
		margin: 12px 0 0;
		padding-top: 10px;
		border-top: 1px solid var(--border-2);
		font-size: 11.5px;
		color: var(--ink-3);
	}

	.legend {
		display: flex;
		align-items: center;
		gap: 14px;
		font-size: 11px;
		color: var(--ink-3);
		padding: 9px 16px;
		border-top: 1px solid var(--border-2);
		background: var(--canvas);
		border-radius: 0 0 13px 13px;
	}

	.legend span {
		display: inline-flex;
		align-items: center;
		gap: 6px;
	}

	.key {
		width: 8px;
		height: 8px;
		border-radius: 2px;
		background: var(--border);
	}

	.key.real {
		background: var(--signal);
	}

	.key.reveal {
		background: var(--code-3xx);
	}

	.key.lost {
		background: var(--code-4xx);
	}
</style>
