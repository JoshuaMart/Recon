<script lang="ts">
	import { enhance } from '$app/forms';
	import { ago, exact } from '$lib/format';
	import { claimed } from '$lib/queue';
	import type { ActionData, PageData } from './$types';

	const { data, form }: { data: PageData; form: ActionData } = $props();

	const program = $derived(data.detail.program);
	const rules = $derived(data.detail.rules);
	const inForce = $derived(rules.filter((rule) => rule.in_force));
	const retired = $derived(rules.filter((rule) => !rule.in_force));

	const answer = $derived(form && 'run' in form ? form.run : undefined);

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
	const command = $derived(
		answer && answer.started === false && answer.args
			? 'docker run --rm ' +
					Object.entries(answer.env ?? {})
						.map(([key, value]) => `-e ${key}=${value}`)
						.join(' ') +
					' fastrecon ' +
					answer.args.join(' ')
			: ''
	);

	/**
	 * What the platform called the execution, which is the only way to find its
	 * logs.
	 *
	 * From the run row first and the form result second, in that order, because
	 * the row survives a reload and the form result does not.
	 */
	const execution = $derived(data.run?.external_id ?? (answer?.started ? answer.external_id : undefined));

	let copied = $state<'no' | 'yes' | 'failed'>('no');
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
	async function copy() {
		clearTimeout(clearing);
		try {
			await navigator.clipboard.writeText(command);
			copied = 'yes';
		} catch {
			copied = 'failed';
		}
		clearing = setTimeout(() => (copied = 'no'), 2000);
	}

	/** The last discovery run, and whether it still holds the program. */
	const scan = $derived(data.run);
	const inFlight = $derived(scan?.state === 'pending' || scan?.state === 'running');
	/** The distinction that matters: a run a scanner opened, or one nothing claimed. */
	const running = $derived(inFlight && Boolean(scan && claimed(scan.state, scan.started_at)));

	/**
	 * The matchers, with what each one means.
	 *
	 * Spelled out on the form because the difference between an apex and an fqdn is
	 * the difference between covering a domain and covering one host, and a rule
	 * that covers less than somebody meant is a perimeter that lies quietly.
	 */
	const matchers = [
		{ value: 'apex', label: 'apex — the domain and everything under it' },
		{ value: 'fqdn', label: 'fqdn — one exact host' },
		{ value: 'cidr', label: 'cidr — an address range' },
		{ value: 'url_prefix', label: 'url prefix — urls starting with this' },
		{ value: 'regex', label: 'regex — matched against the canonical key' }
	];
</script>

<svelte:head><title>{program.name} · recon</title></svelte:head>

<div class="detail">
	<nav class="back"><a href="/programs">← All programs</a></nav>

	<header>
		<span class="state {program.state}">{program.state}</span>
		<h1>{program.name}</h1>
		{#if program.platform_ref}<span class="ref">{program.platform_ref}</span>{/if}
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

	<div class="columns">
		<div class="main">
			<section class="panel">
				<h2>Scope rules in force</h2>

				{#if inForce.length === 0}
					<p class="dim">
						No rule in force. Everything this program discovers lands in the third state: kept, never probed, waiting
						for a decision.
					</p>
				{/if}

				<ul class="rules">
					{#each inForce as rule (rule.id)}
						<li>
							<span class="kind {rule.kind}">{rule.kind}</span>
							<span class="matcher">{rule.matcher}</span>
							<code>{rule.pattern}</code>
							{#if rule.note}<span class="note">{rule.note}</span>{/if}
							<span class="spacer"></span>
							<span class="dim" title={exact(rule.valid_from)}>since {ago(rule.valid_from)}</span>
							<form method="POST" action="?/closeRule" use:enhance>
								<input type="hidden" name="rule_id" value={rule.id} />
								<!-- The version it read. Two concurrent writes
								     losing each other silently is a lost scope. -->
								<input type="hidden" name="version" value={rule.version} />
								<button class="link" type="submit">Retire</button>
							</form>
						</li>
					{/each}
				</ul>
			</section>

			<section class="panel">
				<h2>Add a rule</h2>
				<p class="dim lead">
					Adding a rule reclassifies this program's inventory in the same transaction. Nothing is rescanned.
				</p>

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
					<label class="wide">
						<span>Pattern</span>
						<input class="field" name="pattern" placeholder="example.com" spellcheck="false" />
					</label>
					<label class="wide">
						<span>Note</span>
						<input class="field" name="note" placeholder="why this rule exists" />
					</label>
					<button class="btn btn-signal" type="submit">Add and reclassify</button>
				</form>
			</section>

			{#if retired.length}
				<section class="panel">
					<h2>Retired rules</h2>
					<p class="dim lead">
						A rule is closed and never deleted, so the reason an asset was classified the way it was can still be read.
					</p>
					<ul class="rules">
						{#each retired as rule (rule.id)}
							<li class="past">
								<span class="kind {rule.kind}">{rule.kind}</span>
								<span class="matcher">{rule.matcher}</span>
								<code>{rule.pattern}</code>
								{#if rule.note}<span class="note">{rule.note}</span>{/if}
								<span class="spacer"></span>
								<span class="dim" title={exact(rule.valid_to)}>closed {ago(rule.valid_to)}</span>
							</li>
						{/each}
					</ul>
				</section>
			{/if}
		</div>

		<aside class="side">
			<section class="panel">
				<h2>Discovery</h2>
				<p class="dim lead">
					The cadence covers regular coverage. This covers the case it cannot: relaunching after a scope change.
				</p>

				<form method="POST" action="?/startRun" use:enhance>
					<button class="btn btn-signal start" type="submit" disabled={program.state !== 'active' || inFlight}>
						Start a run
					</button>
				</form>

				{#if program.state !== 'active'}
					<p class="dim note-off">A {program.state} program is not probed, so no run can be opened for it.</p>
				{/if}

				{#if scan}
					<div class="status">
						<div class="line">
							<span class="dot {scan.state}" class:claimed={running}></span>
							<span class="what">
								{#if running}
									Scanning
								{:else if inFlight}
									Waiting for a scanner
								{:else if scan.state === 'failed'}
									Last run failed
								{:else}
									Last run finished
								{/if}
							</span>
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
								{#if inFlight && !running}
									It stays <em>waiting for a scanner</em> until the run opens its target list, which is the only thing that
									says a scanner really took it.
								{/if}
							</p>
						{:else if command}
							<!-- The run is open and nothing has started it. Saying "run started"
							     would be false: with no platform configured the definition is
							     rendered and a person runs the image. -->
							<p class="dim">Run this to start it. The credential inside expires with the run.</p>
							<div class="cmd">
								<code>{command}</code>
								<button type="button" class:done={copied === 'yes'} onclick={copy}>
									{copied === 'yes' ? 'copied' : copied === 'failed' ? 'select it by hand' : 'copy'}
								</button>
							</div>
						{:else if inFlight && !running}
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

						<a class="queue" href="/queue">See the queue</a>
					</div>
				{/if}
			</section>

			<section class="panel">
				<h2>Authorization</h2>
				<dl>
					<dt>From</dt>
					<dd title={exact(program.authorized_from)}>{program.authorized_from.slice(0, 10)}</dd>
					<dt>Until</dt>
					<dd title={exact(program.authorized_to)}>
						{program.authorized_to ? program.authorized_to.slice(0, 10) : 'no end date'}
					</dd>
					{#if program.authorization_ref}
						<dt>Reference</dt>
						<dd>{program.authorization_ref}</dd>
					{/if}
					<dt>Version</dt>
					<dd class="mono">{program.version}</dd>
				</dl>
			</section>

			<section class="panel">
				<h2>Settings</h2>
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
		margin-bottom: 10px;
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
		align-items: baseline;
		gap: 9px;
		margin-bottom: 12px;
	}

	h1 {
		font-size: 16px;
		margin: 0;
		font-weight: 600;
	}

	.state {
		font-size: 10px;
		text-transform: uppercase;
		letter-spacing: 0.06em;
		font-weight: 600;
		color: var(--signal);
	}

	.state.suspended {
		color: var(--code-4xx);
	}

	.state.archived {
		color: var(--dead);
	}

	.ref {
		font-family: var(--font-mono);
		font-size: 11px;
		color: var(--ink-3);
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

	.columns {
		display: grid;
		grid-template-columns: minmax(0, 1fr) 290px;
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
	}

	.panel {
		background: var(--card);
		border: 1px solid var(--border);
		border-radius: var(--radius-card);
		box-shadow: var(--card-shadow);
		padding: 16px 18px 18px;
	}

	h2 {
		font-size: 11px;
		text-transform: uppercase;
		letter-spacing: 0.07em;
		color: var(--ink-3);
		font-weight: 600;
		margin: 0 0 10px;
	}

	.lead {
		margin: 0 0 12px;
		font-size: 12px;
	}

	.rules {
		list-style: none;
		margin: 0;
		padding: 0;
	}

	.rules li {
		display: flex;
		align-items: baseline;
		gap: 8px;
		padding: 6px 0;
		border-bottom: 1px solid var(--border-2);
		font-size: 12px;
		min-width: 0;
	}

	.rules li:last-child {
		border-bottom: 0;
	}

	.rules li.past code {
		color: var(--ink-3);
	}

	.kind {
		font-size: 10px;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		font-weight: 600;
		color: var(--signal);
		flex: none;
	}

	.kind.exclude {
		color: var(--code-5xx);
	}

	.matcher {
		font-size: 10.5px;
		color: var(--ink-3);
		background: var(--canvas);
		border: 1px solid var(--border);
		border-radius: var(--radius-control);
		padding: 1px 5px;
		flex: none;
	}

	code {
		font-family: var(--font-mono);
		font-size: 12px;
		color: var(--ink);
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.note {
		color: var(--ink-3);
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.add,
	.settings {
		display: grid;
		gap: 10px;
	}

	.add {
		grid-template-columns: 1fr 1fr;
	}

	.add .wide {
		grid-column: 1 / -1;
	}

	.add button {
		grid-column: 1 / -1;
		justify-content: center;
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

	.settings button {
		justify-content: center;
	}

	dl {
		display: grid;
		grid-template-columns: 88px 1fr;
		gap: 4px 10px;
		margin: 0;
		font-size: 12px;
	}

	dt {
		color: var(--ink-3);
	}

	dd {
		margin: 0;
		color: var(--ink-2);
	}

	dd.mono {
		font-family: var(--font-mono);
	}

	form {
		margin: 0;
	}

	/*
	 * Its own name, not `wide`. That one already means "span both columns of the
	 * add-a-rule grid" three panels up, and a second meaning in the same
	 * component reached the labels it was never for: they are grid containers, so
	 * a justify-content of center shrank their track to the width of a
	 * placeholder and centred it. Nothing errored, the form just quietly went
	 * narrow.
	 */
	.start {
		width: 100%;
		justify-content: center;
	}

	.note-off {
		margin: 8px 0 0;
		font-size: 12px;
	}

	.status {
		margin-top: 12px;
		border-top: 1px solid var(--border-2);
		padding-top: 12px;
		display: grid;
		gap: 8px;
		font-size: 12px;
	}

	.status p {
		margin: 0;
	}

	.line {
		display: flex;
		align-items: baseline;
		gap: 7px;
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

	.dot.pending {
		background: var(--code-4xx);
	}

	.dot.failed {
		background: var(--code-5xx);
	}

	.dot.claimed {
		background: var(--signal);
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
	 * The panel lives in a 290px column and the command is 700 characters, so the
	 * only layout that keeps the column its own width is a single scrolling row.
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
		padding: 7px 9px;
		font-size: 10.5px;
		color: var(--ink-2);
	}

	.cmd button {
		flex: none;
		border: 0;
		border-left: 1px solid var(--border);
		background: var(--card);
		padding: 0 10px;
		font-size: 11px;
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

	.queue {
		font-size: 11.5px;
		color: var(--ink-3);
		text-decoration: none;
		justify-self: start;
	}

	.queue:hover {
		color: var(--signal);
	}
</style>
