<script lang="ts">
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { api } from '$lib/api';
	import { containers, reload } from '$lib/containers.svelte';
	import { t } from '$lib/i18n';
	import { refresh, session } from '$lib/session.svelte';
	import Logo from '$lib/ui/Logo.svelte';
	import StateDot from '$lib/ui/StateDot.svelte';
	import ThemeSwitch from '$lib/ui/ThemeSwitch.svelte';

	let { children } = $props();
	let ready = $state(false);

	onMount(() => {
		let timer: ReturnType<typeof setInterval>;
		(async () => {
			const me = await refresh();
			if (!me.logged_in || !me.verified) return goto(me.logged_in && me.enroll ? '/enroll' : '/login');
			ready = true;
			await reload();
			// Containers can stop on their own, so the list is refreshed while
			// the tab is in view.
			timer = setInterval(() => document.visibilityState === 'visible' && reload().catch(() => {}), 5000);
		})();
		return () => clearInterval(timer);
	});

	async function logout() {
		await api('POST', '/logout');
		await goto('/login');
	}

	const item = 'flex items-center gap-2.5 rounded-lg px-2 py-1.5 transition hover:bg-hover';
</script>

{#if ready}
	<div class="flex min-h-dvh">
		<aside class="sticky top-0 flex h-dvh w-60 shrink-0 flex-col gap-6 border-r border-line bg-panel px-3 py-5">
			<a href="/" class="flex items-center gap-2.5 px-2">
				<Logo />
				<span class="text-[17px]">{t('app.name')}</span>
			</a>
			<nav class="flex flex-1 flex-col gap-0.5 overflow-y-auto text-[15px]">
				<p class="px-2 pb-1 text-sm text-muted">{t('nav.apps')}</p>
				{#each containers.list as c (c.id)}
					<a
						href="/c/{c.id}"
						class="{item} {page.params.id === c.id ? 'bg-selected' : ''}"
						aria-current={page.params.id === c.id ? 'page' : undefined}
					>
						<StateDot state={c.state} />
						<span class="truncate">{c.id}</span>
					</a>
				{:else}
					{#if containers.loaded}<p class="px-2 text-sm text-muted/70">{t('nav.empty')}</p>{/if}
				{/each}
				<a href="/new" class="{item} mt-1 text-muted hover:text-fg {page.url.pathname === '/new' ? 'bg-selected text-fg' : ''}"
					><span class="w-2 text-center" aria-hidden="true">+</span>{t('nav.new')}</a
				>
			</nav>
			<div class="flex flex-col gap-0.5 text-sm">
				<div class="px-1 pb-2"><ThemeSwitch /></div>
				<p class="truncate px-2 text-muted">{session.me?.email}</p>
				<button class="rounded-lg px-2 py-1.5 text-left text-muted hover:bg-hover hover:text-fg" onclick={logout}
					>{t('nav.logout')}</button
				>
			</div>
		</aside>
		<main class="min-w-0 flex-1 px-10 py-8">
			{@render children()}
		</main>
	</div>
{/if}
