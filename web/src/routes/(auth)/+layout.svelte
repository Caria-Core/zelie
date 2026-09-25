<script lang="ts">
	import Logo from '$lib/ui/Logo.svelte';
	import { t } from '$lib/i18n';

	let { children } = $props();
</script>

<div class="min-h-dvh px-6 py-8">
	<header class="flex items-center gap-2.5">
		<Logo />
		<span class="text-[17px]">{t('app.name')}</span>
		<span class="text-[17px] text-muted">{t('app.by')}</span>
	</header>
	<main class="mx-auto mt-[12vh] flex max-w-sm flex-col gap-8">
		{@render children()}
	</main>
</div>
