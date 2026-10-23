<script lang="ts">
	let { size = 22 }: { size?: number } = $props();
</script>

<svg width={size} height={size} viewBox="0 0 64 64" fill="none" aria-hidden="true">
	<circle cx="32" cy="32" r="27" stroke="var(--accent)" stroke-width="5" />
	<circle cx="32" cy="32" r="15" stroke="var(--accent)" stroke-width="4" stroke-opacity="0.5" />
	<line x1="32" y1="32" x2="55" y2="18.5" stroke="var(--accent)" stroke-width="5" stroke-linecap="round" />
	<circle cx="45" cy="44" r="5" fill="oklch(0.8 0.14 70)" />
</svg>
