<script lang="ts">
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { api } from '$lib/api';
	import { t } from '$lib/i18n';
	import { refresh, session } from '$lib/session.svelte';
	import Logo from '$lib/ui/Logo.svelte';

	let { children } = $props();
	let ready = $state(false);

	onMount(async () => {
		const me = await refresh();
		if (!me.logged_in || !me.verified) return goto(me.logged_in && me.enroll ? '/enroll' : '/login');
		ready = true;
	});

	async function logout() {
		await api('POST', '/logout');
		await goto('/login');
	}
</script>

{#if ready}
	<div class="flex min-h-dvh">
		<aside class="flex w-60 shrink-0 flex-col gap-6 border-r border-line bg-panel px-3 py-5">
			<a href="/" class="flex items-center gap-2.5 px-2">
				<Logo />
				<span class="text-[17px]">{t('app.name')}</span>
			</a>
			<nav class="flex flex-1 flex-col gap-0.5 text-[15px]">
				<p class="px-2 pb-1 text-sm text-muted">{t('nav.apps')}</p>
				<p class="px-2 text-sm text-muted/70">{t('nav.empty')}</p>
			</nav>
			<div class="flex flex-col gap-0.5 text-sm">
				<p class="truncate px-2 text-muted">{session.me?.email}</p>
				<button class="rounded-lg px-2 py-1.5 text-left text-muted hover:bg-hover hover:text-fg" onclick={logout}
					>{t('nav.logout')}</button
				>
			</div>
		</aside>
		<main class="flex-1 px-10 py-8">
			{@render children()}
		</main>
	</div>
{/if}
