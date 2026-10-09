<script lang="ts">
	import { t } from '$lib/i18n';
	import Button from './Button.svelte';

	// What a page shows when what it needs from the panel did not load. The
	// generic error line is left out: the button already says to try again.
	let { message, retry }: { message: string; retry: () => void } = $props();
	let detail = $derived(message === t('common.error') ? '' : message);
</script>

<div class="flex flex-col items-start gap-4">
	<p role="alert" class="leading-snug">
		<span class="text-fg">{t('common.loadFailed')}</span>
		{#if detail}<span class="text-muted">{detail}</span>{/if}
	</p>
	<Button kind="secondary" onclick={retry}>{t('common.retry')}</Button>
</div>
