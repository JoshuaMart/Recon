<script lang="ts">
	import { statusTone } from '#lib/format.ts';

	let { code }: { code: number | null } = $props();
</script>

{#if code !== null}
	<span class="badge num mono {statusTone(code)}">{code}</span>
{/if}
