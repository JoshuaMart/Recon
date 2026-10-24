<script lang="ts">
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { api, ApiError } from '#lib/api.ts';
	import Logo from '#lib/components/Logo.svelte';

	let key = $state('');
	let error = $state('');
	let busy = $state(false);

	async function submit(e: SubmitEvent) {
		e.preventDefault();
		busy = true;
		error = '';
		try {
			await api.post('/api/session', { key });
			const next = page.url.searchParams.get('next');
			// Only a path of this site: never an address handed in through the link.
			await goto(next && next.startsWith('/') && !next.startsWith('//') ? next : '/');
		} catch (err) {
			error = err instanceof ApiError && err.status === 401 ? 'That key is not the one in the config.' : String(err);
		} finally {
			busy = false;
		}
	}
</script>

<svelte:head><title>Sign in · Recon</title></svelte:head>

<div class="wrap">
	<form class="panel" onsubmit={submit}>
		<div class="brand"><Logo size={28} /> Recon</div>
		<label for="key">API key</label>
		<input id="key" class="input mono" type="password" autocomplete="current-password" bind:value={key} required />
		{#if error}<p class="error" role="alert">{error}</p>{/if}
		<button class="btn btn-primary" type="submit" disabled={busy || !key}>Sign in</button>
		<p class="muted hint">The key set as <code class="mono">api_key</code> in the config. The session lasts 30 days.</p>
	</form>
</div>

<style>
	.wrap {
		min-height: 100vh;
		display: grid;
		place-items: center;
		padding: 16px;
	}
	form {
		width: min(380px, 100%);
		padding: 24px;
		gap: 10px;
		box-shadow: var(--shadow);
	}
	.brand {
		display: flex;
		align-items: center;
		gap: 10px;
		font-weight: 600;
		font-size: 18px;
		margin-bottom: 8px;
	}
	label {
		font-size: 13px;
		color: var(--muted);
	}
	.btn {
		justify-content: center;
	}
	.hint {
		margin: 0;
		font-size: 12px;
	}
</style>
