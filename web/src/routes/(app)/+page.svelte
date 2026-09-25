<script lang="ts">
	import { goto } from '$app/navigation';
	import { containers } from '$lib/containers.svelte';
	import { t } from '$lib/i18n';
	import Button from '$lib/ui/Button.svelte';
	import Lead from '$lib/ui/Lead.svelte';

	const running = $derived(containers.list.filter((c) => c.state === 'running').length);
</script>

{#if containers.loaded}
	<div class="flex max-w-xl flex-col items-start gap-6">
		{#if containers.list.length === 0}
			<Lead title={t('home.title')} text={' ' + t('home.lead')} />
			<Button onclick={() => goto('/new')}>{t('nav.new')}</Button>
		{:else}
			<Lead title={t('home.count', { n: running })} text={' ' + t('home.countLead')} />
		{/if}
	</div>
{/if}
