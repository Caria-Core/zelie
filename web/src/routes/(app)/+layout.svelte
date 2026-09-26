<script lang="ts">
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { api } from '$lib/api';
	import { apps, reload, shownState } from '$lib/apps.svelte';
	import { t } from '$lib/i18n';
	import { refresh, session } from '$lib/session.svelte';
	import AppIcon from '$lib/ui/AppIcon.svelte';
	import ConfirmDialog from '$lib/ui/ConfirmDialog.svelte';
	import Logo from '$lib/ui/Logo.svelte';
	import StateDot from '$lib/ui/StateDot.svelte';
	import ThemeSwitch from '$lib/ui/ThemeSwitch.svelte';

	let { children } = $props();
	let ready = $state(false);
	// On narrow screens the sidebar is a menu that opens over the page.
	let menu = $state(false);

	$effect(() => {
		page.url.pathname;
		menu = false;
	});

	onMount(() => {
		let timer: ReturnType<typeof setInterval>;
		(async () => {
			const me = await refresh();
			if (!me.logged_in || !me.verified) return goto(me.logged_in && me.enroll ? '/enroll' : '/login');
			ready = true;
			await reload();
			// Apps can stop on their own and deployments move on, so the list
			// is refreshed while the tab is in view.
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
	<div class="flex min-h-dvh flex-col md:flex-row">
		<header class="flex items-center justify-between border-b border-line px-4 py-3 md:hidden">
			<a href="/" class="flex items-center gap-2.5">
				<Logo />
				<span class="text-[17px]">{t('app.name')}</span>
			</a>
			<button class="rounded-lg px-2 py-1 text-sm text-muted hover:text-fg" aria-expanded={menu} onclick={() => (menu = !menu)}
				>{t('nav.menu')}</button
			>
		</header>
		<aside
			class="{menu
				? 'fixed inset-0 top-[53px] z-10 flex'
				: 'hidden'} flex-col gap-6 bg-panel px-3 py-5 md:sticky md:top-0 md:flex md:h-dvh md:w-60 md:shrink-0 md:border-r md:border-line"
		>
			<a href="/" class="hidden items-center gap-2.5 px-2 md:flex">
				<Logo />
				<span class="text-[17px]">{t('app.name')}</span>
			</a>
			<nav class="flex flex-1 flex-col gap-0.5 overflow-y-auto text-[15px]">
				<p class="px-2 pb-1 text-sm text-muted">{t('nav.apps')}</p>
				{#each apps.list as a (a.id)}
					<a
						href="/a/{a.id}"
						class="{item} {page.params.app === a.id ? 'bg-selected' : ''}"
						aria-current={page.params.app === a.id ? 'page' : undefined}
					>
						<AppIcon source={a.source} size="sm" />
						<span class="min-w-0 flex-1 truncate">{a.id}</span>
						<StateDot state={shownState(a)} />
					</a>
				{:else}
					{#if apps.loaded}<p class="px-2 text-sm text-muted/70">{t('nav.empty')}</p>{/if}
				{/each}
				<a href="/new" class="{item} mt-1 text-muted hover:text-fg {page.url.pathname === '/new' ? 'bg-selected text-fg' : ''}"
					><span class="w-2 text-center" aria-hidden="true">+</span>{t('nav.new')}</a
				>
			</nav>
			<div class="flex flex-col gap-0.5 text-sm">
				<div class="px-1 pb-2"><ThemeSwitch /></div>
				<a
					href="/github"
					class="rounded-lg px-2 py-1.5 text-muted hover:bg-hover hover:text-fg {page.url.pathname.startsWith('/github')
						? 'bg-selected text-fg'
						: ''}">{t('nav.github')}</a
				>
				<a
					href="/account"
					class="truncate rounded-lg px-2 py-1.5 text-muted hover:bg-hover hover:text-fg {page.url.pathname === '/account'
						? 'bg-selected text-fg'
						: ''}"
					title={t('nav.account')}>{session.me?.email}</a
				>
				<button class="rounded-lg px-2 py-1.5 text-left text-muted hover:bg-hover hover:text-fg" onclick={logout}
					>{t('nav.logout')}</button
				>
			</div>
		</aside>
		<main class="min-w-0 flex-1 px-4 py-6 md:px-10 md:py-8">
			{@render children()}
		</main>
	</div>
	<ConfirmDialog />
{/if}
