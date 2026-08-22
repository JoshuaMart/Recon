<script lang="ts">
	import { enhance } from '$app/forms';
	import { ago, exact } from '$lib/format';
	import { authorisation, coverage, runStatus } from '$lib/program';
	import { depthsByProgram, lastDiscoveryRuns } from '$lib/queue';
	import { programHref } from '$lib/query';
	import type { ActionData, PageData } from './$types';

	const { data, form }: { data: PageData; form: ActionData } = $props();

	/**
	 * The form is open when somebody asked for it, and always when there is nothing
	 * to list, since there is nothing else to do on this page then.
	 *
	 * Derived rather than initialised from `data`: a state seeded from a prop only
	 * ever reads its first value, so after a client-side navigation the form would
	 * stay closed on an organisation with no program.
	 */
	let adding = $state(false);
	const open = $derived(adding || data.programs.length === 0);

	/**
	 * The queue, folded the way this page reads it: one entry per program.
	 *
	 * Both folds are pure and tested, and both tolerate an empty answer, which is
	 * what a row gets when the queue could not be read. The regions then draw
	 * their own absence rather than a zero, because a queue nobody could reach is
	 * not a queue at rest.
	 */
	const runs = $derived(lastDiscoveryRuns(data.runs));
	const depths = $derived(depthsByProgram(data.depths));
	const reachable = $derived(data.runs.length > 0 || data.depths.length > 0);

	/** How many perimeters are being walked right now, said once at the top. */
	const busy = $derived(data.programs.filter((program) => runStatus(runs.get(program.id))?.inFlight).length);
</script>

<svelte:head><title>Programs · recon</title></svelte:head>

<div class="programs">
	<header>
		<h1>Programs</h1>
		<span class="dim">
			{data.programs.length} in this organisation{busy > 0 ? `, ${busy} with a run open` : ''}
		</span>
		<span class="spacer"></span>
		{#if !open}
			<button class="btn" type="button" onclick={() => (adding = true)}>New program</button>
		{/if}
	</header>

	{#if form?.message}
		<p class="notice">{form.message}</p>
	{/if}

	{#if data.programs.length === 0 && !form?.message}
		<p class="empty">
			No program yet. A program is an authorised perimeter, and it is what everything else hangs from: nothing is
			scanned without one.
		</p>
	{/if}

	{#if open}
		<section class="panel">
			<h2>New program</h2>
			<form method="POST" action="?/create" class="create" use:enhance>
				<label class="wide">
					<span>Name</span>
					<input class="field" name="name" placeholder="example.com" spellcheck="false" required />
				</label>

				<!-- The authorisation reference first among the optional fields, and not
				     buried at the end. The permission to scan is a first-class
				     datum: this is where the answer to "who said we could" is recorded,
				     and a program without it is one nobody can justify later. -->
				<label class="wide">
					<span>Authorisation reference</span>
					<input
						class="field"
						name="authorization_ref"
						placeholder="the bug bounty policy, a signed engagement, owning the domain"
					/>
				</label>

				<label>
					<span>Authorised until</span>
					<input class="field" name="authorized_to" type="date" />
					<!-- An expired authorization turns a program suspended, so an
					     empty date is a permission with no end rather than an omission. -->
					<em>Empty means no end date.</em>
				</label>

				<label>
					<span>Rate limit</span>
					<input class="field" name="rate_limit_rps" type="number" min="1" max="1000" placeholder="10" />
					<em>Requests per second, shared by every worker.</em>
				</label>

				<label class="wide">
					<span>Platform reference</span>
					<input class="field" name="platform_ref" placeholder="hackerone:example" spellcheck="false" />
					<!-- Descriptive only, never a join key between organizations. -->
					<em>Descriptive. Two organisations following the same target stay separate.</em>
				</label>

				<div class="actions">
					<button class="btn btn-signal" type="submit">Create</button>
					{#if data.programs.length > 0}
						<button class="link" type="button" onclick={() => (adding = false)}>Cancel</button>
					{/if}
				</div>
			</form>
		</section>
	{/if}

	<ul>
		{#each data.programs as program (program.id)}
			{@const auth = authorisation(program.authorized_to)}
			{@const run = runStatus(runs.get(program.id))}
			{@const depth = depths.get(program.id)}
			{@const rules = program.rules_in_force ?? 0}
			<li class:archived={program.state === 'archived'}>
				<div class="head">
					<!-- A dot rather than the word, because `active` on every row is a
					     word that says nothing. The exception keeps the word: a
					     suspended or archived program is the one worth reading. -->
					<span class="state-dot {program.state}" title={program.state}></span>
					<a class="name" href="/programs/{program.id}">{program.name}</a>
					{#if program.platform_ref}
						<span class="ref">{program.platform_ref}</span>
					{/if}
					{#if program.state !== 'active'}
						<span class="state {program.state}">{program.state}</span>
					{/if}
					<span class="spacer"></span>
					<span class="auth {auth.tone}" title={exact(program.authorized_to)}>{auth.label}</span>
					<a class="open" href="/programs/{program.id}" aria-label="Open {program.name}">
						<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round">
							<path d="M9 6l6 6-6 6" />
						</svg>
					</a>
				</div>

				<!--
					The three questions somebody opens this page to ask, in the width the
					row used to leave empty: how big is the perimeter, is anything running
					on it, is anything waiting.
				-->
				<div class="regions">
					<div class="region scope">
						<div class="lbl">Scope</div>
						{#if rules === 0}
							<div class="figure"><span class="none">no rule in force</span></div>
							<p class="third">everything discovered is kept and never probed</p>
						{:else}
							<div class="figure">
								<b>{program.assets_in_scope ?? 0}</b>
								<span class="unit">in scope</span>
								<span class="of">of {program.assets ?? 0} known</span>
							</div>
							<div class="bar"><i style:width="{coverage(program)}%"></i></div>
							<div class="meta">
								<span><b>{rules}</b> {rules === 1 ? 'rule' : 'rules'} in force</span>
								<span class="sep">·</span>
								<span><b>{program.rate_limit_rps}</b> rps</span>
								<span class="sep">·</span>
								<!-- The assets of a program are the search with a filter, not a second
								     list with its own rules . The link carries the filter, which it
								     did not until program_id became a searchable field: it went to an
								     unfiltered list, so the label said one thing and the page showed
								     another. -->
								<a class="link" href={programHref(program.id)}>Search its assets</a>
							</div>
						{/if}
					</div>

					<div class="region">
						<div class="lbl">Discovery</div>
						{#if program.state !== 'active'}
							<div class="line"><span class="dot"></span><span class="what dim">Not probed</span></div>
							<p class="sub">a {program.state} program keeps its data and stops being observed</p>
						{:else if !reachable}
							<div class="figure"><span class="none">the queue could not be read</span></div>
						{:else if !run}
							<div class="figure"><span class="none">never run</span></div>
							<p class="sub">a discovery run starts from the page of this program</p>
						{:else}
							{@const scan = runs.get(program.id)!}
							<div class="line">
								<span class="dot {run.tone}"></span>
								<span class="what">{run.label}</span>
								<span class="dim" title={exact(scan.finished_at ?? scan.started_at ?? scan.created_at)}>
									{ago(scan.finished_at ?? scan.started_at ?? scan.created_at)}
								</span>
							</div>
							{#if run.stalled}
								<p class="sub warn">nothing has claimed it</p>
							{:else if scan.error}
								<p class="sub why">{scan.error}</p>
							{:else if scan.observations > 0}
								<p class="sub"><b>{scan.observations}</b> observations</p>
							{/if}
						{/if}
					</div>

					<div class="region">
						<div class="lbl">Queue</div>
						{#if !reachable}
							<p class="quiet">no answer from the queue</p>
						{:else if !depth || depth.due + depth.later + depth.in_run === 0}
							<p class="quiet">nothing is scheduled</p>
						{:else}
							<div class="counts">
								<span class="count" class:on={depth.due > 0}>{depth.due}<em>due</em></span>
								<span class="count">{depth.later}<em>scheduled</em></span>
								<span class="count" class:flight={depth.in_run > 0}>{depth.in_run}<em>in flight</em></span>
							</div>
						{/if}
					</div>
				</div>
			</li>
		{/each}
	</ul>
</div>

<style>
	.programs {
		padding: 16px 18px 60px;
		min-width: 0;
	}

	header {
		display: flex;
		align-items: baseline;
		gap: 10px;
		margin-bottom: 14px;
	}

	h1 {
		font-size: 16px;
		margin: 0;
		font-weight: 600;
		letter-spacing: -0.01em;
	}

	.panel {
		background: var(--card);
		border: 1px solid var(--border);
		border-radius: var(--radius-card);
		box-shadow: var(--card-shadow);
		padding: 16px 18px 18px;
		margin-bottom: 10px;
	}

	h2 {
		font-size: 11px;
		text-transform: uppercase;
		letter-spacing: 0.07em;
		color: var(--ink-3);
		font-weight: 600;
		margin: 0 0 12px;
	}

	.create {
		display: grid;
		grid-template-columns: 1fr 1fr;
		gap: 10px;
	}

	.create .wide,
	.create .actions {
		grid-column: 1 / -1;
	}

	.actions {
		display: flex;
		align-items: center;
		gap: 12px;
	}

	label {
		display: grid;
		gap: 4px;
		min-width: 0;
	}

	label > span {
		font-size: 10.5px;
		text-transform: uppercase;
		letter-spacing: 0.06em;
		color: var(--ink-3);
		font-weight: 600;
	}

	label em {
		font-size: 11px;
		font-style: normal;
		color: var(--ink-3);
	}

	input.field {
		font-family: var(--font-sans);
	}

	input[name='name'],
	input[name='platform_ref'] {
		font-family: var(--font-mono);
	}

	.empty {
		background: var(--card);
		border: 1px dashed var(--border);
		border-radius: var(--radius-card);
		padding: 18px;
		color: var(--ink-3);
		font-size: 12.5px;
		margin: 0;
	}

	ul {
		list-style: none;
		margin: 0;
		padding: 0;
		display: grid;
		gap: 9px;
	}

	li {
		background: var(--card);
		border: 1px solid var(--border);
		border-radius: var(--radius-card);
		box-shadow: var(--card-shadow);
		padding: 12px 14px 13px;
	}

	/* Dim the card, never the words: `archived` is the one thing this row is
	   saying, and an opacity over the subtree takes it below readable. */
	li.archived {
		background: #fcfdfd;
	}

	li.archived .name {
		color: var(--ink-2);
	}

	li.archived .bar i {
		background: var(--dead);
	}

	.head {
		display: flex;
		align-items: baseline;
		gap: 9px;
		min-width: 0;
	}

	.state-dot {
		width: 7px;
		height: 7px;
		border-radius: 50%;
		background: var(--signal);
		flex: none;
		transform: translateY(-1px);
	}

	.state-dot.suspended {
		background: var(--code-4xx);
	}

	.state-dot.archived {
		background: var(--dead);
	}

	.state {
		font-size: 10px;
		text-transform: uppercase;
		letter-spacing: 0.06em;
		font-weight: 600;
		color: var(--code-4xx);
		flex: none;
	}

	.state.archived {
		color: var(--ink-3);
	}

	.name {
		font-size: 15px;
		font-weight: 600;
		color: var(--ink);
		text-decoration: none;
		letter-spacing: -0.01em;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.name:hover {
		text-decoration: underline;
		text-decoration-color: var(--signal);
	}

	.ref {
		font-family: var(--font-mono);
		font-size: 10.5px;
		color: var(--ink-3);
		background: var(--canvas);
		border: 1px solid var(--border);
		border-radius: var(--radius-control);
		padding: 1px 5px;
		flex: none;
	}

	.auth {
		font-size: 11.5px;
		color: var(--ink-3);
		flex: none;
	}

	.auth.soon {
		color: var(--code-4xx);
		font-weight: 500;
	}

	.auth.expired {
		color: var(--code-5xx);
		font-weight: 500;
	}

	.open {
		color: var(--ink-3);
		display: inline-flex;
		align-items: center;
		flex: none;
		transform: translateY(2px);
	}

	.open:hover {
		color: var(--ink);
	}

	.regions {
		display: grid;
		grid-template-columns: minmax(0, 1fr) 296px 296px;
		margin-top: 11px;
		border-top: 1px solid var(--border-2);
		padding-top: 11px;
	}

	/* A row narrow enough that three regions would crush the counts stacks them,
	   in the order they are read. */
	@media (max-width: 1080px) {
		.regions {
			grid-template-columns: minmax(0, 1fr);
			gap: 11px;
		}

		.region + .region {
			border-left: 0;
			border-top: 1px solid var(--border-2);
			padding: 11px 0 0;
		}
	}

	.region {
		min-width: 0;
	}

	.region:not(:last-child) {
		padding-right: 20px;
	}

	.region + .region {
		border-left: 1px solid var(--border-2);
		padding-left: 20px;
	}

	.lbl {
		font-size: 9.5px;
		text-transform: uppercase;
		letter-spacing: 0.08em;
		color: var(--ink-3);
		font-weight: 600;
	}

	.figure {
		display: flex;
		align-items: baseline;
		gap: 6px;
		margin-top: 6px;
		min-width: 0;
	}

	.figure b {
		font-family: var(--font-mono);
		font-size: 19px;
		font-weight: 600;
	}

	.figure .unit {
		font-size: 11.5px;
		font-weight: 500;
		color: var(--ink-3);
	}

	.figure .of {
		font-size: 11.5px;
		color: var(--ink-3);
	}

	/* An absence is not a number, so it is not set in the numeric face. The same
	   rule the asset view's tiles follow. */
	.figure .none {
		font-size: 13.5px;
		font-weight: 500;
		font-style: italic;
		color: var(--ink-3);
	}

	.bar {
		height: 3px;
		border-radius: 2px;
		background: var(--border);
		overflow: hidden;
		margin-top: 7px;
		max-width: 320px;
	}

	.bar i {
		display: block;
		height: 100%;
		background: var(--signal);
	}

	.meta {
		display: flex;
		align-items: baseline;
		gap: 7px;
		margin-top: 8px;
		font-size: 12px;
		color: var(--ink-2);
	}

	.meta b {
		font-family: var(--font-mono);
		font-weight: 600;
	}

	.sep {
		color: var(--border);
	}

	.third {
		font-size: 11.5px;
		color: var(--code-4xx);
		margin: 8px 0 0;
	}

	.line {
		display: flex;
		align-items: baseline;
		gap: 7px;
		margin-top: 7px;
		font-size: 12.5px;
	}

	.what {
		font-weight: 500;
	}

	/* The state as a dot, on the same vocabulary as the queue: amber is a job
	   nobody claimed, green is a run a scanner opened, grey is done. */
	.dot {
		width: 6px;
		height: 6px;
		border-radius: 50%;
		background: var(--dead);
		flex: none;
		transform: translateY(-1px);
	}

	.dot.running {
		background: var(--signal);
	}

	.dot.pending {
		background: var(--code-4xx);
	}

	.dot.failed {
		background: var(--code-5xx);
	}

	.sub {
		font-size: 11.5px;
		color: var(--ink-3);
		margin: 3px 0 0;
	}

	.sub b {
		font-family: var(--font-mono);
		font-weight: 600;
		color: var(--ink-2);
	}

	.sub.warn {
		color: var(--code-4xx);
	}

	.sub.why {
		font-family: var(--font-mono);
		font-size: 10.5px;
		color: var(--code-5xx);
		overflow-wrap: anywhere;
	}

	.counts {
		display: flex;
		gap: 26px;
		margin-top: 6px;
	}

	.count {
		font-family: var(--font-mono);
		font-size: 17px;
		color: var(--ink-3);
	}

	.count em {
		display: block;
		font-family: var(--font-sans);
		font-style: normal;
		font-size: 9.5px;
		text-transform: uppercase;
		letter-spacing: 0.06em;
		color: var(--ink-3);
		font-weight: 600;
	}

	.count.on {
		color: var(--ink);
	}

	.count.flight {
		color: var(--signal);
	}

	.quiet {
		font-size: 11.5px;
		color: var(--ink-3);
		margin: 9px 0 0;
		font-style: italic;
	}
</style>
