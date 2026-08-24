<script lang="ts">
	import { enhance } from '$app/forms';
	import { ago, exact } from '$lib/format';
	import { authorisation, coverage, runStatus } from '$lib/program';
	import { programHref } from '$lib/query';
	import type { ActionData, PageData } from './$types';

	const { data, form }: { data: PageData; form: ActionData } = $props();

	const program = $derived(data.detail.program);
	const rules = $derived(data.detail.rules);
	const inForce = $derived(rules.filter((rule) => rule.in_force));
	const excludes = $derived(inForce.filter((rule) => rule.kind === 'exclude').length);

	/**
	 * Whether a discovery run has anything to enumerate.
	 *
	 * A run's perimeter is the apex includes and nothing else: the other matchers
	 * narrow or exclude what enumeration finds, they cannot tell it where to
	 * start. A programme whose only include is a `url_prefix` or a `cidr`
	 * therefore has a rule in force, reads as configured, and answers 409 the
	 * moment somebody presses the button.
	 *
	 * Said before the click rather than after it, and not as a refusal: adding an
	 * apex second is an ordinary way to build a perimeter, so the panel states
	 * what is missing and leaves the button alone.
	 */
	const apexes = $derived(inForce.filter((rule) => rule.kind === 'include' && rule.matcher === 'apex'));
	const retired = $derived(rules.filter((rule) => !rule.in_force));

	const answer = $derived(form && 'run' in form ? form.run : undefined);
	const auth = $derived(authorisation(program.authorized_to));

	/**
	 * The command that runs the definition the control plane handed back, and it
	 * exists only where nothing else will run it.
	 *
	 * The answer carries the arguments on both paths, because a definition is
	 * worth showing whether or not it was started, and reading them without
	 * reading `started` is what put "run this to start it" under a run the
	 * platform had already launched. The screen then said the opposite of what
	 * had happened, on the one panel whose job is to say which of the two it was.
	 *
	 * Assembled here rather than sent: the control plane knows nothing about how
	 * the image is invoked on the machine somebody is reading this on. It lives
	 * for as long as the form result does, since the credential inside is signed
	 * and never stored, and a reload leaves the run without its command, which the
	 * panel says rather than hides.
	 */
	const answered = $derived(answer?.runs ?? []);
	const commandFor = (run: (typeof answered)[number]) =>
		run.args
			? 'docker run --rm ' +
				Object.entries(run.env ?? {})
					.map(([key, value]) => `-e ${key}=${value}`)
					.join(' ') +
				' fastrecon ' +
				run.args.join(' ')
			: '';

	/**
	 * What the platform called the execution, which is the only way to find its
	 * logs.
	 *
	 * From the run row first and the form result second, in that order, because
	 * the row survives a reload and the form result does not.
	 */
	const execution = $derived(answered.length === 0 ? data.run?.external_id : undefined);

	// Per run rather than one flag: a perimeter of three apexes hands over three
	// commands, and one shared flag would report the copy on all of them.
	let copied = $state<Record<number, 'yes' | 'failed'>>({});
	let clearing: ReturnType<typeof setTimeout> | undefined;

	/**
	 * The clipboard is not always there.
	 *
	 * `navigator.clipboard` is undefined outside a secure context, so a console
	 * served over plain http to anything but localhost throws here. Unguarded the
	 * button silently stayed on "copy", on the one screen whose whole purpose is
	 * handing a command over, and the answer to "why is nothing happening" was in
	 * a console nobody had open. It says so instead, and the command is still on
	 * screen to select by hand.
	 */
	async function copy(text: string, index: number) {
		clearTimeout(clearing);
		try {
			await navigator.clipboard.writeText(text);
			copied = { [index]: 'yes' };
		} catch {
			copied = { [index]: 'failed' };
		}
		clearing = setTimeout(() => (copied = {}), 2000);
	}

	/**
	 * The last discovery run, read the way the list reads it.
	 *
	 * Shared rather than restated: both screens make the same claim about the same
	 * run, and the distinction that matters, a run nothing has claimed against one
	 * a scanner opened, is exactly the kind that diverges when it is written twice.
	 */
	const scan = $derived(data.run);
	const status = $derived(runStatus(scan));
	const inFlight = $derived(status?.inFlight ?? false);

	/** The queue of this program, all three schedules together. */
	const depth = $derived(data.depth);
	const waiting = $derived(depth ? depth.due + depth.later + depth.in_run : 0);

	/**
	 * The matchers, with what each one means.
	 *
	 * Spelled out on the form because the difference between an apex and an fqdn is
	 * the difference between covering a domain and covering one host, and a rule
	 * that covers less than somebody meant is a perimeter that lies quietly.
	 */
	const matchers = [
		{ value: 'apex', label: 'apex · the domain and everything under it' },
		{ value: 'fqdn', label: 'fqdn · one exact host' },
		{ value: 'cidr', label: 'cidr · an address range' },
		{ value: 'url_prefix', label: 'url prefix · urls starting with this' },
		{ value: 'regex', label: 'regex · matched against the canonical key' }
	];
</script>

<svelte:head><title>{program.name} · recon</title></svelte:head>

<div class="detail">
	<nav class="back"><a href="/programs">← All programs</a></nav>

	<header>
		<span class="state-dot {program.state}" title={program.state}></span>
		<h1>{program.name}</h1>
		{#if program.platform_ref}<span class="ref">{program.platform_ref}</span>{/if}
		{#if program.state !== 'active'}<span class="state {program.state}">{program.state}</span>{/if}
		<span class="spacer"></span>
		<a class="btn" href={programHref(program.id)}>Search its assets</a>
	</header>

	{#if form && 'message' in form && form.message}
		<p class="notice">
			{form.message}
			{#if form.stale}
				<br />Somebody else changed this while you had it open. Reload before writing again.
			{/if}
		</p>
	{/if}

	{#if form && 'effect' in form && form.effect}
		<!-- The reclassification committed with the write, so what moved is part
		     of the answer rather than something to go looking for. -->
		<p class="effect">
			<b>{form.effect.changed}</b> of {form.effect.examined} assets changed classification.
			{#if form.effect.gained > 0 || form.effect.lost > 0}
				{form.effect.gained} came into scope, {form.effect.lost} left it, and the due dates moved with them.
			{/if}
			Nothing was rescanned.
		</p>
	{/if}

	<!--
		The general information block this kind of tool opens with, on the asset
		view's tile vocabulary. It used to be three sidebar panels of dead text
		beside a form that took the whole page.
	-->
	<div class="facts">
		<div class="tile">
			<div class="lbl">In scope</div>
			<div class="value">
				<span class="v">{program.assets_in_scope ?? 0}</span>
				<span class="unit">of {program.assets ?? 0} known</span>
			</div>
			<div class="track"><i style:width="{coverage(program)}%"></i></div>
			<!-- The gap is not one thing: a rule excludes an asset, or no rule
			     settles it and it is kept and never probed. The bar draws the gap
			     and the sentence refuses to name a cause it cannot see. -->
			<div class="sub">
				{#if (program.assets ?? 0) === 0}
					nothing discovered yet
				{:else if (program.assets_in_scope ?? 0) === (program.assets ?? 0)}
					every known asset matches a rule
				{:else}
					{(program.assets ?? 0) - (program.assets_in_scope ?? 0)} excluded or undecided
				{/if}
			</div>
		</div>

		<div class="tile">
			<div class="lbl">Rules in force</div>
			<div class="value">
				<span class="v" class:none={inForce.length === 0}>{inForce.length === 0 ? 'none' : inForce.length}</span>
			</div>
			<div class="sub">
				{#if inForce.length === 0}
					everything discovered is kept and never probed
				{:else if excludes === 0}
					no exclude, so nothing is carved out
				{:else}
					{excludes} of them carve something out
				{/if}
			</div>
		</div>

		<div class="tile">
			<div class="lbl">Rate limit</div>
			<div class="value"><span class="v">{program.rate_limit_rps}</span><span class="unit">rps</span></div>
			<div class="sub">shared by every worker</div>
		</div>

		<div class="tile">
			<div class="lbl">Authorised</div>
			<div class="value">
				<span class="v {auth.tone}" class:none={!program.authorized_to} title={exact(program.authorized_to)}>
					{auth.label}
				</span>
			</div>
			<div class="sub" title={exact(program.authorized_from)}>since {program.authorized_from.slice(0, 10)}</div>
		</div>

		<div class="tile">
			<div class="lbl">Last run</div>
			{#if !data.reachable}
				<div class="value"><span class="v none">not known</span></div>
				<div class="sub">the queue could not be read</div>
			{:else if !scan}
				<div class="value"><span class="v none">never</span></div>
				<div class="sub">no discovery run has been opened</div>
			{:else}
				<div class="value">
					<span class="v" title={exact(scan.finished_at ?? scan.started_at ?? scan.created_at)}>
						{ago(scan.finished_at ?? scan.started_at ?? scan.created_at)}
					</span>
				</div>
				<div class="sub">{scan.observations} observations, every {program.discovery_interval}</div>
			{/if}
		</div>
	</div>

	<div class="columns">
		<div class="main">
			<!-- The scope is the program, so it gets the room. The form that writes
			     into it is a row at the foot of the table, not a panel larger than
			     what it edits. -->
			<section class="panel">
				<div class="phead">
					<h2>Scope</h2>
					{#if inForce.length === 0}
						<span class="note">
							No rule in force. Everything this program discovers lands in the third state: kept, never probed, waiting
							for a decision.
						</span>
					{:else}
						<span class="note">
							{inForce.length}
							{inForce.length === 1 ? 'rule' : 'rules'} in force. A rule is closed and never deleted, so the reason an asset
							was classified the way it was can still be read.
						</span>
					{/if}
				</div>

				{#if inForce.length > 0}
					<table>
						<thead>
							<tr>
								<th>Kind</th>
								<th>Matcher</th>
								<th>Pattern</th>
								<th>Note</th>
								<th>Since</th>
								<th></th>
							</tr>
						</thead>
						<tbody>
							{#each inForce as rule (rule.id)}
								<tr>
									<td><span class="kind {rule.kind}">{rule.kind}</span></td>
									<td><span class="matcher">{rule.matcher}</span></td>
									<td><code>{rule.pattern}</code></td>
									<td class="rule-note">{rule.note ?? ''}</td>
									<td class="since" title={exact(rule.valid_from)}>{ago(rule.valid_from)}</td>
									<td>
										<form method="POST" action="?/closeRule" use:enhance>
											<input type="hidden" name="rule_id" value={rule.id} />
											<!-- The version it read. Two concurrent writes
											     losing each other silently is a lost scope. -->
											<input type="hidden" name="version" value={rule.version} />
											<button class="link" type="submit">Retire</button>
										</form>
									</td>
								</tr>
							{/each}
						</tbody>
					</table>
				{/if}

				<div class="compose">
					<form method="POST" action="?/addRule" class="add" use:enhance>
						<label>
							<span>Kind</span>
							<select class="field" name="kind">
								<option value="include">include</option>
								<option value="exclude">exclude</option>
							</select>
						</label>
						<label>
							<span>Matcher</span>
							<select class="field" name="matcher">
								{#each matchers as matcher (matcher.value)}
									<option value={matcher.value}>{matcher.label}</option>
								{/each}
							</select>
						</label>
						<label>
							<span>Pattern</span>
							<input class="field" name="pattern" placeholder="example.com" spellcheck="false" />
						</label>
						<label>
							<span>Note</span>
							<input class="field" name="note" placeholder="why this rule exists" />
						</label>
						<button class="btn btn-signal" type="submit">Add and reclassify</button>
					</form>
					<p class="after">The inventory is reclassified in the same transaction. Nothing is rescanned.</p>
				</div>

				{#if retired.length}
					<!-- Closed rules are history and not work: folded away, and native,
					     so it opens without javascript. -->
					<details class="retired">
						<summary>{retired.length} retired {retired.length === 1 ? 'rule' : 'rules'}</summary>
						<table>
							<tbody>
								{#each retired as rule (rule.id)}
									<tr class="past">
										<td><span class="kind {rule.kind}">{rule.kind}</span></td>
										<td><span class="matcher">{rule.matcher}</span></td>
										<td><code>{rule.pattern}</code></td>
										<td class="rule-note">{rule.note ?? ''}</td>
										<td class="since" title={exact(rule.valid_to)}>closed {ago(rule.valid_to)}</td>
										<td></td>
									</tr>
								{/each}
							</tbody>
						</table>
					</details>
				{/if}
			</section>

			<!-- Out of the 290px column it used to sit in: the command it hands over
			     is seven hundred characters, and a gutter made it scroll sideways. -->
			<section class="panel">
				<div class="phead">
					<h2>Discovery</h2>
					<span class="note">
						The cadence covers regular coverage. This covers the case it cannot: relaunching after a scope change.
					</span>
				</div>
				<div class="pbody">
					{#if apexes.length === 0}
						<p class="warn">
							No apex include, so a run has nothing to enumerate. The other matchers narrow what enumeration finds; they
							cannot say where it starts. Add an <code>apex</code> rule with the domain, without a
							<code>*.</code> prefix: it covers the domain and everything under it.
						</p>
					{/if}

					{#if program.state !== 'active'}
						<p class="warn">A {program.state} program is not probed, so no run can be opened for it.</p>
					{/if}

					<div class="run">
						<form method="POST" action="?/startRun" use:enhance>
							<button class="btn btn-signal" type="submit" disabled={program.state !== 'active' || inFlight}>
								Start a run
							</button>
						</form>
						<span class="lead">One discovery run at a time, per apex.</span>
					</div>

					{#if !data.reachable}
						<p class="lead offline">
							The queue could not be read, so this panel cannot say whether a run is open. Starting one is still refused
							if it is.
						</p>
					{:else if scan && status}
						<div class="status">
							<div class="line">
								<span class="dot {status.tone}"></span>
								<span class="what">{status.label}</span>
								<span class="spacer"></span>
								<span class="dim" title={exact(scan.finished_at ?? scan.started_at ?? scan.created_at)}>
									{ago(scan.finished_at ?? scan.started_at ?? scan.created_at)}
								</span>
							</div>

							{#if scan.observations > 0}
								<p class="count"><b>{scan.observations}</b> observations</p>
							{/if}

							{#if execution}
								<!-- The platform started it, so the panel says so and hands over
								     the one identifier that finds the execution's logs. Offering a
								     command here would invite somebody to run the same perimeter
								     twice.

								     The sentence about waiting is bounded by the state, because it
								     is only true while the run is in flight: written under a run
								     that has finished it describes something that is no longer
								     happening, which is the same fault as offering a command for a
								     run the platform already took. -->
								<p class="dim">
									Started on the platform as <code class="ext">{execution}</code>.
									{#if status.stalled}
										It stays <em>waiting for a scanner</em> until the run opens its target list, which is the only thing that
										says a scanner really took it.
									{/if}
								</p>
							{:else if answered.length}
								<!-- One entry per run, because a perimeter of several apexes is
								     several runs: the scanner takes one root domain per execution.
								     Each says what happened to its own, since a platform that
								     refused the third started the first two. -->
								{#each answered as run, i (run.run_id)}
									{#if run.started && run.external_id}
										<p class="dim">
											{#if run.apex}<code class="ext">{run.apex}</code> started{:else}Started{/if} on the platform as
											<code class="ext">{run.external_id}</code>.
										</p>
									{:else if run.args}
										<!-- The run is open and nothing has started it. Saying "run
										     started" would be false: with no platform configured the
										     definition is rendered and a person runs the image. -->
										<p class="dim">
											Run this to start {#if run.apex}<code class="ext">{run.apex}</code>{:else}it{/if}. The credential
											inside expires with the run.
										</p>
										<div class="cmd">
											<code>{commandFor(run)}</code>
											<button type="button" class:done={copied[i] === 'yes'} onclick={() => copy(commandFor(run), i)}>
												{copied[i] === 'yes' ? 'copied' : copied[i] === 'failed' ? 'select it by hand' : 'copy'}
											</button>
										</div>
									{/if}
								{/each}
							{:else if status.stalled}
								<!-- The token was minted once and never stored, so this page
								     cannot hand it back. Say where it went rather than pretend. -->
								<p class="dim">
									Its credential was signed once and never stored, so it cannot be shown again. The deadline sweeper
									expires the run and the program is free to start another.
								</p>
							{/if}

							{#if scan.error}
								<p class="why">{scan.error}</p>
							{/if}
						</div>
					{/if}
				</div>
			</section>
		</div>

		<aside class="side">
			<!-- Authorisation and settings were two boxes about one subject: what
			     this program is set to do, and on whose say-so. -->
			<section class="panel">
				<div class="phead"><h2>Program</h2></div>
				<div class="pbody">
					<form method="POST" action="?/updateProgram" class="settings" use:enhance>
						<input type="hidden" name="version" value={program.version} />
						<!-- The write states the program rather than patching the fields
						     somebody touched: a partial write against an optimistic lock is
						     the shape where two edits that never overlapped still lose each
						     other. -->
						<input type="hidden" name="name" value={program.name} />
						<input type="hidden" name="discovery_interval" value={program.discovery_interval} />
						<input type="hidden" name="authorized_from" value={program.authorized_from} />
						<input type="hidden" name="authorized_to" value={program.authorized_to ?? ''} />
						<input type="hidden" name="platform" value={program.platform ?? ''} />
						<input type="hidden" name="platform_ref" value={program.platform_ref ?? ''} />
						<input type="hidden" name="authorization_ref" value={program.authorization_ref ?? ''} />
						<label>
							<span>State</span>
							<select class="field" name="state">
								<!-- A suspended program keeps its data and stops being probed,
								     and its assets do not change lifecycle. They did not become
								     inactive, they stopped being observed. -->
								<option value="active" selected={program.state === 'active'}>active</option>
								<option value="suspended" selected={program.state === 'suspended'}>suspended</option>
								<option value="archived" selected={program.state === 'archived'}>archived</option>
							</select>
						</label>
						<label>
							<span>Rate limit</span>
							<input
								class="field"
								name="rate_limit_rps"
								type="number"
								min="1"
								max="1000"
								value={program.rate_limit_rps}
							/>
						</label>
						<button class="btn" type="submit">Save</button>
					</form>

					<div class="hr"></div>

					<dl>
						<dt>Authorised</dt>
						<dd title={exact(program.authorized_from)}>{program.authorized_from.slice(0, 10)}</dd>
						<dt>Until</dt>
						<dd class:none={!program.authorized_to} title={exact(program.authorized_to)}>
							{program.authorized_to ? program.authorized_to.slice(0, 10) : 'no end date'}
						</dd>
						{#if program.authorization_ref}
							<dt>Reference</dt>
							<dd>{program.authorization_ref}</dd>
						{/if}
						<dt>Cadence</dt>
						<dd>every {program.discovery_interval}</dd>
						<dt>Version</dt>
						<dd class="mono">{program.version}</dd>
					</dl>
				</div>
			</section>

			<section class="panel">
				<div class="phead"><h2>Queue</h2></div>
				<div class="pbody">
					{#if !data.reachable}
						<p class="quiet">no answer from the queue</p>
					{:else if waiting === 0}
						<p class="quiet">nothing is scheduled</p>
					{:else}
						<!-- One number over three schedules, so the panel says which
						     three rather than letting it read as a single queue. -->
						<p class="qnote">resolve, full probe and fingerprint together</p>
						<div class="qcounts">
							<span class="qcount" class:on={depth!.due > 0}>{depth!.due}<em>due</em></span>
							<span class="qcount">{depth!.later}<em>scheduled</em></span>
							<span class="qcount" class:flight={depth!.in_run > 0}>{depth!.in_run}<em>in flight</em></span>
						</div>
					{/if}
					<a class="queue" href="/queue">See the queue</a>
				</div>
			</section>
		</aside>
	</div>
</div>

<style>
	.detail {
		padding: 14px 18px 60px;
		min-width: 0;
	}

	.back {
		margin-bottom: 9px;
	}

	.back a {
		font-size: 12px;
		color: var(--ink-3);
		text-decoration: none;
	}

	.back a:hover {
		color: var(--ink);
	}

	header {
		display: flex;
		align-items: center;
		gap: 9px;
		margin-bottom: 12px;
	}

	h1 {
		font-size: 19px;
		margin: 0;
		font-weight: 600;
		letter-spacing: -0.015em;
	}

	.state-dot {
		width: 8px;
		height: 8px;
		border-radius: 50%;
		background: var(--signal);
		flex: none;
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
	}

	.state.archived {
		color: var(--ink-3);
	}

	.ref {
		font-family: var(--font-mono);
		font-size: 11px;
		color: var(--ink-3);
		background: var(--canvas);
		border: 1px solid var(--border);
		border-radius: var(--radius-control);
		padding: 2px 6px;
	}

	.effect {
		background: var(--signal-bg);
		border-left: 2px solid var(--signal);
		border-radius: var(--radius-control);
		padding: 7px 10px;
		font-size: 12px;
		color: #0a7a58;
		margin: 0 0 10px;
	}

	.effect b {
		font-family: var(--font-mono);
	}

	/* ---- the facts ---- */

	.facts {
		display: grid;
		grid-template-columns: repeat(auto-fit, minmax(196px, 1fr));
		gap: 10px;
		margin-bottom: 12px;
	}

	.tile {
		background: var(--card);
		border: 1px solid var(--border);
		border-radius: var(--radius-card);
		box-shadow: var(--card-shadow);
		padding: 9px 12px 11px;
		min-width: 0;
		overflow: hidden;
	}

	.lbl {
		font-size: 9.5px;
		text-transform: uppercase;
		letter-spacing: 0.08em;
		color: var(--ink-3);
		font-weight: 600;
	}

	.value {
		display: flex;
		align-items: baseline;
		gap: 6px;
		margin-top: 5px;
		min-width: 0;
	}

	.v {
		font-family: var(--font-mono);
		font-size: 18px;
		font-weight: 600;
		color: var(--ink);
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	/* An absence is not a number, so it is not set in the numeric face. */
	.v.none {
		font-family: var(--font-sans);
		font-size: 13.5px;
		font-weight: 500;
		font-style: italic;
		color: var(--ink-3);
	}

	.v.soon {
		font-family: var(--font-sans);
		font-size: 14px;
		color: var(--code-4xx);
	}

	.v.expired {
		font-family: var(--font-sans);
		font-size: 14px;
		color: var(--code-5xx);
	}

	.unit {
		font-size: 11.5px;
		font-weight: 500;
		color: var(--ink-3);
	}

	.tile .sub {
		font-size: 11px;
		color: var(--ink-3);
		margin-top: 3px;
		line-height: 1.35;
		overflow-wrap: anywhere;
	}

	/* Not `dv-bar`. That prefix means the asset view's shared sheet, which this
	   page does not import, so the name would promise a rule that is elsewhere. */
	.track {
		height: 3px;
		margin-top: 8px;
		border-radius: 2px;
		background: var(--border);
		overflow: hidden;
	}

	.track i {
		display: block;
		height: 100%;
		background: var(--signal);
	}

	/* ---- two columns ---- */

	.columns {
		display: grid;
		grid-template-columns: minmax(0, 1fr) 300px;
		gap: 12px;
		align-items: start;
	}

	@media (max-width: 1000px) {
		.columns {
			grid-template-columns: minmax(0, 1fr);
		}
	}

	.main,
	.side {
		display: grid;
		gap: 12px;
		min-width: 0;
		align-content: start;
	}

	.panel {
		background: var(--card);
		border: 1px solid var(--border);
		border-radius: var(--radius-card);
		box-shadow: var(--card-shadow);
		min-width: 0;
	}

	.phead {
		display: flex;
		align-items: baseline;
		gap: 9px;
		padding: 12px 16px 11px;
		border-bottom: 1px solid var(--border-2);
		min-width: 0;
	}

	.phead h2 {
		font-size: 11px;
		text-transform: uppercase;
		letter-spacing: 0.07em;
		color: var(--ink-3);
		font-weight: 600;
		margin: 0;
		flex: none;
	}

	.phead .note {
		font-size: 11.5px;
		color: var(--ink-3);
		min-width: 0;
	}

	.pbody {
		padding: 13px 16px 15px;
	}

	/* ---- the rules ---- */

	table {
		width: 100%;
		border-collapse: collapse;
		font-size: 12.5px;
	}

	th {
		text-align: left;
		font-size: 10px;
		text-transform: uppercase;
		letter-spacing: 0.06em;
		color: var(--ink-3);
		font-weight: 600;
		padding: 9px 10px 7px;
		border-bottom: 1px solid var(--border-2);
	}

	td {
		padding: 9px 10px;
		border-bottom: 1px solid var(--border-2);
		color: var(--ink-2);
		vertical-align: baseline;
	}

	tbody tr:last-child td {
		border-bottom: 0;
	}

	th:first-child,
	td:first-child {
		padding-left: 16px;
	}

	th:last-child,
	td:last-child {
		padding-right: 16px;
		text-align: right;
	}

	.kind {
		font-size: 10px;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		font-weight: 600;
		color: var(--signal);
	}

	.kind.exclude {
		color: var(--code-5xx);
	}

	.matcher {
		font-family: var(--font-mono);
		font-size: 10px;
		text-transform: uppercase;
		letter-spacing: 0.04em;
		color: var(--ink-2);
		background: var(--canvas);
		border: 1px solid var(--border);
		border-radius: var(--radius-control);
		padding: 1px 5px;
		white-space: nowrap;
	}

	td code {
		font-family: var(--font-mono);
		font-size: 12px;
		color: var(--ink);
		overflow-wrap: anywhere;
	}

	.past code {
		color: var(--ink-3);
	}

	.rule-note {
		color: var(--ink-3);
	}

	.since {
		color: var(--ink-3);
		white-space: nowrap;
	}

	/* ---- the composer ---- */

	.compose {
		border-top: 1px solid var(--border-2);
		background: var(--canvas);
		padding: 11px 16px 12px;
	}

	.add {
		display: grid;
		grid-template-columns: 132px 300px minmax(0, 1fr) minmax(0, 1fr) auto;
		gap: 8px;
		align-items: end;
	}

	/* A column too narrow for five fields wraps them into two rows rather than
	   crushing the matcher, whose labels are the point of it. */
	@media (max-width: 1240px) {
		.add {
			grid-template-columns: 132px minmax(0, 1fr);
		}

		.add button {
			grid-column: 1 / -1;
			justify-content: center;
		}
	}

	.after {
		font-size: 11.5px;
		color: var(--ink-3);
		margin: 9px 0 0;
	}

	label {
		display: grid;
		gap: 4px;
		min-width: 0;
	}

	label span {
		font-size: 10.5px;
		text-transform: uppercase;
		letter-spacing: 0.06em;
		color: var(--ink-3);
		font-weight: 600;
	}

	select.field,
	input.field {
		font-family: var(--font-sans);
	}

	input[name='pattern'] {
		font-family: var(--font-mono);
	}

	.retired {
		border-top: 1px solid var(--border-2);
	}

	.retired summary {
		padding: 10px 16px;
		font-size: 12px;
		color: var(--ink-3);
		cursor: pointer;
	}

	.retired summary:hover {
		color: var(--ink);
	}

	.retired td {
		border-top: 1px solid var(--border-2);
		border-bottom: 0;
	}

	/* ---- discovery ---- */

	.warn {
		background: var(--code-4xx-bg);
		border-left: 2px solid var(--code-4xx);
		border-radius: var(--radius-control);
		padding: 8px 11px;
		font-size: 12px;
		color: #8a5a0e;
		margin: 0 0 12px;
	}

	.warn code {
		font-family: var(--font-mono);
		background: #fff;
		border-radius: 2px;
		padding: 0 3px;
	}

	.run {
		display: flex;
		align-items: center;
		gap: 12px;
	}

	.lead {
		font-size: 12px;
		color: var(--ink-3);
		margin: 0;
	}

	.offline {
		margin-top: 12px;
	}

	.status {
		margin-top: 13px;
		border-top: 1px solid var(--border-2);
		padding-top: 12px;
		display: grid;
		gap: 8px;
		font-size: 12.5px;
	}

	.status p {
		margin: 0;
	}

	.line {
		display: flex;
		align-items: center;
		gap: 8px;
	}

	/* The state as a dot, on the same vocabulary as the queue: amber is a job
	   nobody claimed, green is a run a scanner opened, grey is done. */
	.dot {
		width: 7px;
		height: 7px;
		border-radius: 50%;
		background: var(--dead);
		flex: none;
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

	.what {
		font-weight: 500;
	}

	.count b {
		font-family: var(--font-mono);
		font-size: 15px;
		font-weight: 500;
	}

	/*
	 * One line that scrolls sideways, rather than a token wrapped over twenty.
	 * The panel has the width of the main column now, so the command is readable
	 * rather than a slot four words wide.
	 */
	.cmd {
		display: flex;
		align-items: stretch;
		border: 1px solid var(--border);
		border-radius: var(--radius-control);
		background: var(--canvas);
		overflow: hidden;
	}

	.ext {
		font-family: var(--font-mono);
		font-size: 11px;
		color: var(--ink-2);
	}

	.cmd code {
		flex: 1;
		min-width: 0;
		overflow-x: auto;
		white-space: nowrap;
		padding: 8px 10px;
		font-size: 11px;
		color: var(--ink-2);
	}

	.cmd button {
		flex: none;
		border: 0;
		border-left: 1px solid var(--border);
		background: var(--card);
		padding: 0 12px;
		font-size: 11.5px;
		color: var(--ink-2);
	}

	.cmd button:hover {
		color: var(--ink);
	}

	.cmd button.done {
		background: var(--signal);
		border-left-color: var(--signal);
		color: #fff;
		font-weight: 500;
	}

	.why {
		font-family: var(--font-mono);
		font-size: 10.5px;
		color: var(--code-5xx);
		overflow-wrap: anywhere;
	}

	/* ---- the sidebar ---- */

	.settings {
		display: grid;
		gap: 11px;
	}

	.settings button {
		justify-content: center;
	}

	.hr {
		border-top: 1px solid var(--border-2);
		margin: 13px 0;
	}

	dl {
		display: grid;
		grid-template-columns: 88px minmax(0, 1fr);
		gap: 6px 10px;
		margin: 0;
		font-size: 12px;
	}

	dt {
		color: var(--ink-3);
	}

	dd {
		margin: 0;
		color: var(--ink-2);
		overflow-wrap: anywhere;
	}

	dd.none {
		font-style: italic;
		color: var(--ink-3);
	}

	dd.mono {
		font-family: var(--font-mono);
	}

	form {
		margin: 0;
	}

	.qnote {
		font-size: 11px;
		color: var(--ink-3);
		margin: 0 0 7px;
	}

	.qcounts {
		display: grid;
		grid-template-columns: repeat(3, minmax(0, 1fr));
		gap: 10px;
	}

	/* Its own name, not `count`. That one already means the observation total
	   under the run status in this component, and a second meaning reached it:
	   the paragraph took the mono face at 17px, so the word rendered larger than
	   the number beside it. */
	.qcount {
		font-family: var(--font-mono);
		font-size: 17px;
		color: var(--ink-3);
	}

	.qcount em {
		display: block;
		font-family: var(--font-sans);
		font-style: normal;
		font-size: 9.5px;
		text-transform: uppercase;
		letter-spacing: 0.06em;
		color: var(--ink-3);
		font-weight: 600;
	}

	.qcount.on {
		color: var(--ink);
	}

	.qcount.flight {
		color: var(--signal);
	}

	.quiet {
		font-size: 11.5px;
		color: var(--ink-3);
		font-style: italic;
		margin: 0;
	}

	.queue {
		font-size: 11.5px;
		color: var(--ink-3);
		text-decoration: none;
		justify-self: start;
		display: inline-block;
		margin-top: 11px;
	}

	.queue:hover {
		color: var(--signal);
	}
</style>
