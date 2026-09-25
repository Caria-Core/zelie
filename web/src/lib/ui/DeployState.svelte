<script lang="ts">
	import { t, type Key } from '$lib/i18n';
	import type { Deployment } from '$lib/apps.svelte';

	let { state }: { state: Deployment['state'] } = $props();
	const styles: Record<Deployment['state'], string> = {
		queued: 'bg-selected text-muted',
		building: 'bg-selected text-fg',
		starting: 'bg-selected text-fg',
		live: 'bg-ok/15 text-ok',
		failed: 'bg-danger/15 text-danger',
		replaced: 'bg-hover text-muted'
	};
	const moving = $derived(state === 'queued' || state === 'building' || state === 'starting');
</script>

<span class="inline-flex items-center gap-1.5 rounded-full px-2 py-0.5 text-xs font-medium {styles[state]}">
	{#if moving}<span class="size-1.5 animate-pulse rounded-full bg-current" aria-hidden="true"></span>{/if}
	{t(`deploy.${state}` as Key)}
</span>
