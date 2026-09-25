<script lang="ts">
	import { onMount } from 'svelte';
	import { page } from '$app/state';
	import { api } from '$lib/api';
	import { messageOf } from '$lib/errors';
	import { t } from '$lib/i18n';
	import ErrorText from '$lib/ui/ErrorText.svelte';
	import Lead from '$lib/ui/Lead.svelte';

	// GitHub sends the browser here after the App is created. The code in the
	// address is traded for the App's keys, then the App is installed.
	let error = $state('');

	onMount(async () => {
		const q = page.url.searchParams;
		try {
			const out = await api<{ install_url: string }>('POST', '/github/app', { code: q.get('code') ?? '', state: q.get('state') ?? '' });
			location.replace(out.install_url);
		} catch (err) {
			error = messageOf(err) || t('common.error');
		}
	});
</script>

<div class="flex max-w-md flex-col gap-4">
	{#if error}
		<Lead title={t('github.createdFailed')} />
		<ErrorText message={error} />
		<a href="/github" class="text-sm text-muted hover:text-fg">{t('github.back')}</a>
	{:else}
		<Lead title={t('github.finishing')} />
	{/if}
</div>
