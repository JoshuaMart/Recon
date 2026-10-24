<script lang="ts">
	import { label } from '#lib/format.ts';

	let { state }: { state: string } = $props();
	const tone: Record<string, string> = {
		active: 'ok',
		candidate: 'info',
		flapping: 'warn',
		inactive: 'bad',
		unobservable: 'warn',
		archived: ''
	};
</script>

<span class="badge {tone[state] ?? ''}"><span class="dot" aria-hidden="true"></span>{label(state)}</span>
