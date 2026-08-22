<script lang="ts">
	import { enhance } from '$app/forms';
	import type { ActionData } from './$types';

	const { form }: { form: ActionData } = $props();
</script>

<svelte:head><title>Connect · recon</title></svelte:head>

<main>
	<div class="panel">
		<div class="mark">r</div>
		<h1>recon</h1>
		<p class="lead">Paste an organisation token. It stays on the server and the browser never sees it again.</p>

		<form method="POST" use:enhance>
			<label class="label" for="token">Token</label>
			<!-- No autofocus. It is tempting on a one-field screen and it moves a
			     screen reader past the text that explains what the field wants. -->
			<input class="field" id="token" name="token" type="password" autocomplete="off" spellcheck="false" />

			{#if form?.message}
				<p class="notice">{form.message}</p>
			{/if}

			<button class="btn btn-signal" type="submit">Connect</button>
		</form>

		<p class="foot">
			Tokens are created and revoked in the database. Revoking one there ends this session at the next request.
		</p>
	</div>
</main>

<style>
	main {
		min-height: 100vh;
		display: grid;
		place-items: center;
		padding: 24px;
	}

	.panel {
		background: var(--card);
		border: 1px solid var(--border);
		border-radius: var(--radius-card);
		box-shadow: var(--card-shadow);
		padding: 26px 24px 20px;
		width: 100%;
		max-width: 360px;
	}

	.mark {
		width: 30px;
		height: 30px;
		border-radius: 9px;
		background: var(--signal);
		color: #fff;
		display: grid;
		place-items: center;
		font-weight: 700;
	}

	h1 {
		font-size: 19px;
		margin: 12px 0 4px;
		letter-spacing: -0.01em;
	}

	.lead {
		margin: 0 0 18px;
		color: var(--ink-3);
		font-size: 12.5px;
	}

	.label {
		display: block;
		font-size: 10.5px;
		text-transform: uppercase;
		letter-spacing: 0.07em;
		color: var(--ink-3);
		font-weight: 600;
		margin-bottom: 5px;
	}

	form {
		display: grid;
		gap: 10px;
	}

	form .btn {
		justify-content: center;
	}

	.foot {
		margin: 18px 0 0;
		padding-top: 12px;
		border-top: 1px solid var(--border-2);
		color: var(--ink-3);
		font-size: 11.5px;
	}
</style>
