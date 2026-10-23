<script lang="ts">
	// A text field that narrows a list as you type. The empty value is "any".
	interface Option {
		value: string;
		label: string;
		hint?: string;
	}

	let {
		options,
		value = $bindable(''),
		label,
		placeholder = 'Any',
		id
	}: { options: Option[]; value?: string; label: string; placeholder?: string; id: string } = $props();

	let text = $state('');
	let open = $state(false);
	let active = $state(0);
	let input: HTMLInputElement;

	const selected = $derived(options.find((o) => o.value === value));
	// The field shows the choice while it is not being edited.
	$effect(() => {
		if (!open) text = selected?.label ?? '';
	});

	const matches = $derived.by(() => {
		const t = text.trim().toLowerCase();
		if (!open || !t || t === selected?.label.toLowerCase()) return options.slice(0, 50);
		const starts = options.filter((o) => o.label.toLowerCase().startsWith(t));
		const has = options.filter((o) => !o.label.toLowerCase().startsWith(t) && (o.label + ' ' + (o.hint ?? '')).toLowerCase().includes(t));
		return [...starts, ...has].slice(0, 50);
	});

	function choose(o: Option | undefined) {
		value = o?.value ?? '';
		open = false;
		text = o?.label ?? '';
	}

	function onkeydown(e: KeyboardEvent) {
		if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
			e.preventDefault();
			if (!open) {
				open = true;
				active = 0;
				return;
			}
			const n = matches.length;
			if (n) active = (active + (e.key === 'ArrowDown' ? 1 : n - 1)) % n;
			document.getElementById(`${id}-opt-${active}`)?.scrollIntoView({ block: 'nearest' });
		} else if (e.key === 'Enter' && open) {
			e.preventDefault();
			if (text.trim() === '') choose(undefined);
			else if (matches[active]) choose(matches[active]);
		} else if (e.key === 'Escape' && open) {
			e.preventDefault();
			open = false;
		}
	}

	function onblur() {
		// Typed text that names nothing is not a filter: it goes back to the choice.
		if (text.trim() === '') value = '';
		open = false;
	}
</script>

<div class="combo">
	<label for={id} class="sr-only">{label}</label>
	<input
		{id}
		bind:this={input}
		class="input"
		role="combobox"
		autocomplete="off"
		spellcheck="false"
		aria-expanded={open}
		aria-controls="{id}-list"
		aria-autocomplete="list"
		aria-activedescendant={open && matches.length ? `${id}-opt-${active}` : undefined}
		{placeholder}
		bind:value={text}
		onfocus={() => {
			open = true;
			active = 0;
			input.select();
		}}
		oninput={() => {
			open = true;
			active = 0;
		}}
		{onkeydown}
		{onblur}
	/>
	{#if value}
		<button class="clear" type="button" aria-label="Clear {label.toLowerCase()}" onclick={() => choose(undefined)}>
			<svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" aria-hidden="true"
				><path d="M6 6l12 12M18 6 6 18" /></svg
			>
		</button>
	{/if}
	{#if open}
		<ul id="{id}-list" role="listbox" aria-label={label}>
			{#each matches as o, i (o.value)}
				<li
					id="{id}-opt-{i}"
					role="option"
					aria-selected={o.value === value}
					class:active={i === active}
					onmousedown={(e) => {
						// Before blur, so the click is not lost.
						e.preventDefault();
						choose(o);
					}}
					onmousemove={() => (active = i)}
				>
					<span>{o.label}</span>
					{#if o.hint}<span class="hint">{o.hint}</span>{/if}
				</li>
			{:else}
				<li class="none" role="presentation">No match</li>
			{/each}
		</ul>
	{/if}
</div>

<style>
	.combo {
		position: relative;
		width: 280px;
		max-width: 100%;
	}
	input {
		width: 100%;
		padding-right: 30px;
	}
	.clear {
		position: absolute;
		right: 6px;
		top: 50%;
		translate: 0 -50%;
		width: 22px;
		height: 22px;
		border: 0;
		border-radius: 999px;
		background: transparent;
		color: var(--faint);
		display: grid;
		place-items: center;
		cursor: pointer;
		padding: 0;
	}
	ul {
		position: absolute;
		z-index: 20;
		top: calc(100% + 6px);
		left: 0;
		right: 0;
		min-width: 280px;
		max-height: 320px;
		overflow-y: auto;
		margin: 0;
		padding: 4px;
		list-style: none;
		border-radius: 10px;
		background: var(--raised);
		box-shadow: var(--shadow);
	}
	li {
		display: flex;
		justify-content: space-between;
		gap: 12px;
		padding: 7px 10px;
		border-radius: 6px;
		font-size: 13px;
		cursor: pointer;
	}
	li.active {
		background: var(--control-hover);
	}
	li[aria-selected='true'] {
		color: var(--accent-text);
	}
	.hint {
		color: var(--faint);
		font-size: 12px;
		white-space: nowrap;
	}
	.none {
		color: var(--faint);
		cursor: default;
	}
	@media (hover: hover) {
		.clear:hover {
			color: var(--text-strong);
			background: var(--pressed);
		}
	}
</style>
