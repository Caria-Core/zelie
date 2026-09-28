<script lang="ts">
	import { Database } from '@lucide/svelte';
	import { current } from '$lib/current.svelte';
	import { t } from '$lib/i18n';
	import RedisData from '$lib/ui/RedisData.svelte';
	import TableData from '$lib/ui/TableData.svelte';

	const app = $derived(current.app!);
	const appId = $derived(app.id);
	const running = $derived(app.state === 'running');
</script>

<div class="flex flex-col gap-6">
	<div class="max-w-2xl">
		<h2 class="font-medium">{t('viewer.title')}</h2>
		<p class="text-sm text-muted">{t(app.engine === 'redis' ? 'viewer.leadRedis' : 'viewer.lead', { db: appId })}</p>
	</div>
	{#if !running}
		<div class="flex flex-col items-start gap-3 rounded-2xl border border-dashed border-line p-5">
			<span class="grid size-10 place-items-center rounded-xl bg-selected"><Database size={20} strokeWidth={1.75} /></span>
			<p class="text-sm text-muted">{t('viewer.notRunning', { db: appId })}</p>
		</div>
	{:else if app.engine === 'redis'}
		{#key appId}<RedisData db={appId} />{/key}
	{:else}
		{#key appId}<TableData db={appId} />{/key}
	{/if}
</div>
