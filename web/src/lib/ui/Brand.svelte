<script lang="ts">
	import { t } from '$lib/i18n';
	import Logo from './Logo.svelte';

	// home is where the name leads; empty for none, on pages outside the
	// panel such as the login.
	let { home = '/', class: cls = '' }: { home?: string; class?: string } = $props();
</script>

<span class="flex items-center gap-2.5 text-[17px] {cls}">
	{#if home}
		<a href={home} class="flex items-center gap-2.5"><Logo /><span>{t('app.name')}</span></a>
	{:else}
		<Logo /><span>{t('app.name')}</span>
	{/if}
	<a href="https://cariacore.com" target="_blank" rel="noopener" class="text-muted transition hover:text-fg">{t('app.by')}</a>
</span>
