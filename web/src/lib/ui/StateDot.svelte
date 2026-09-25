<script lang="ts">
	import { t, type Key } from '$lib/i18n';

	let { state, label = false }: { state: string; label?: boolean } = $props();
	const known = ['running', 'stopping', 'stopped', 'created', 'none'];
	const key = $derived(`state.${known.includes(state) ? state : 'unknown'}` as Key);
	const colour = $derived(state === 'running' ? 'bg-ok' : state === 'created' || state === 'stopping' ? 'bg-muted' : 'bg-line');
</script>

<span class="inline-flex items-center gap-1.5">
	<span class="size-2 rounded-full {colour}" aria-hidden="true"></span>
	{#if label}<span>{t(key)}</span>{:else}<span class="sr-only">{t(key)}</span>{/if}
</span>
