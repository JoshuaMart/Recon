<script lang="ts">
	import { enhance } from '$app/forms';
	import { ago, exact } from '$lib/format';
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
	 * An authorisation that has closed, or is about to.
	 *
	 * The automatic active to suspended transition at expiry comes with an alert
	 * seven days out. Neither exists yet, so the screen says it rather than letting
	 * a programme look active while its authorisation has run out.
	 */
	function authorisation(to: string | undefined): { label: string; tone: string } {
		if (!to) return { label: 'no end date', tone: 'plain' };
		const days = (Date.parse(to) - Date.now()) / 86400000;
		if (days < 0) return { label: 'expired ' + ago(to), tone: 'expired' };
		if (days < 7) return { label: 'ends in ' + Math.ceil(days) + ' days', tone: 'soon' };
		return { label: 'until ' + to.slice(0, 10), tone: 'plain' };
	}
</script>

<svelte:head><title>Programs · recon</title></svelte:head>

<div class="programs">
	<header>
		<h1>Programs</h1>
		<span class="dim">{data.programs.length} in this organisation</span>
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
			<li>
				<div class="head">
					<span class="state {program.state}">{program.state}</span>
					<a class="name" href="/programs/{program.id}">{program.name}</a>
					{#if program.platform_ref}
						<span class="ref">{program.platform_ref}</span>
					{/if}
					<span class="spacer"></span>
					<span class="auth {auth.tone}" title={exact(program.authorized_to)}>{auth.label}</span>
				</div>

				<div class="counts">
					<span><b>{program.assets_in_scope ?? 0}</b> in scope</span>
					<span class="dim">of {program.assets ?? 0} known</span>
					<span class="sep">·</span>
					<span><b>{program.rules_in_force ?? 0}</b> scope rules</span>
					<span class="sep">·</span>
					<span><b>{program.rate_limit_rps}</b> rps</span>
					<span class="spacer"></span>
					<!-- The assets of a program are the search with a filter, not a second
					     list with its own rules . The link carries the filter, which it
					     did not until program_id became a searchable field: it went to an
					     unfiltered list, so the label said one thing and the page showed
					     another. -->
					<a class="link" href={programHref(program.id)}>Search its assets</a>
				</div>
			</li>
		{/each}
	</ul>
</div>

<style>
	.programs {
		padding: 14px 18px 60px;
		min-width: 0;
	}

	header {
		display: flex;
		align-items: baseline;
		gap: 10px;
		margin-bottom: 12px;
	}

	h1 {
		font-size: 15px;
		margin: 0;
		font-weight: 600;
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
	}

	li {
		background: var(--card);
		border: 1px solid var(--border);
		border-radius: var(--radius-card);
		box-shadow: var(--card-shadow);
		padding: 12px 14px;
		margin-bottom: 8px;
	}

	.head {
		display: flex;
		align-items: baseline;
		gap: 9px;
		min-width: 0;
	}

	.state {
		font-size: 10px;
		text-transform: uppercase;
		letter-spacing: 0.06em;
		font-weight: 600;
		color: var(--signal);
		flex: none;
	}

	.state.suspended {
		color: var(--code-4xx);
	}

	.state.archived {
		color: var(--dead);
	}

	.name {
		font-size: 14px;
		font-weight: 500;
		color: var(--ink);
		text-decoration: none;
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
		font-size: 11px;
		color: var(--ink-3);
	}

	.auth {
		font-size: 11.5px;
		color: var(--ink-3);
		flex: none;
	}

	.auth.soon {
		color: var(--code-4xx);
	}

	.auth.expired {
		color: var(--code-5xx);
		font-weight: 500;
	}

	.counts {
		display: flex;
		align-items: baseline;
		gap: 7px;
		margin-top: 6px;
		font-size: 12px;
		color: var(--ink-2);
	}

	.counts b {
		font-family: var(--font-mono);
		font-weight: 600;
	}

	.sep {
		color: var(--border);
	}
</style>
