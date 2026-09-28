<script lang="ts">
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { api } from '$lib/api';
	import { apps, isDatabase, reload, shownState } from '$lib/apps.svelte';
	import { t } from '$lib/i18n';
	import { loadServer, server } from '$lib/server.svelte';
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
			loadServer().catch(() => {});
			// Apps can stop on their own and deployments move on, so the list
			// is refreshed while the tab is in view. A new release is rare;
			// the panel looks for one once a day.
			let ticks = 0;
			timer = setInterval(() => {
				if (document.visibilityState !== 'visible') return;
				reload().catch(() => {});
				if (++ticks % 120 === 0) loadServer().catch(() => {});
			}, 5000);
		})();
		return () => clearInterval(timer);
	});

	async function logout() {
		await api('POST', '/logout');
		await goto('/login');
	}

	const item = 'flex items-center gap-2.5 rounded-lg px-2 py-1.5 transition hover:bg-hover';
	const groups = $derived([
		{ title: t('nav.apps'), list: apps.list.filter((a) => !isDatabase(a)), href: '/new', add: t('nav.new') },
		{ title: t('nav.databases'), list: apps.list.filter(isDatabase), href: '/new/database', add: t('nav.newDatabase') }
	]);
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
			<nav class="flex flex-1 flex-col gap-6 overflow-y-auto text-[15px]">
				{#each groups as g (g.href)}
					<div class="flex flex-col gap-0.5">
						<p class="px-2 pb-1 text-sm text-muted">{g.title}</p>
						{#each g.list as a (a.id)}
							<a
								href="/a/{a.id}"
								class="{item} {page.params.app === a.id ? 'bg-selected' : ''}"
								aria-current={page.params.app === a.id ? 'page' : undefined}
							>
								<AppIcon source={a.source} database={isDatabase(a)} size="sm" />
								<span class="min-w-0 flex-1 truncate">{a.id}</span>
								<StateDot state={shownState(a)} />
							</a>
						{:else}
							{#if apps.loaded}<p class="px-2 text-sm text-muted/70">{t('nav.empty')}</p>{/if}
						{/each}
						<a
							href={g.href}
							class="{item} mt-1 text-muted hover:text-fg {page.url.pathname === g.href ? 'bg-selected text-fg' : ''}"
							><span class="w-2 text-center" aria-hidden="true">+</span>{g.add}</a
						>
					</div>
				{/each}
			</nav>
			<div class="flex flex-col gap-0.5 text-sm">
				<div class="px-1 pb-2"><ThemeSwitch /></div>
				<a
					href="/server"
					class="flex items-center justify-between gap-2 rounded-lg px-2 py-1.5 text-muted hover:bg-hover hover:text-fg {page.url.pathname === '/server'
						? 'bg-selected text-fg'
						: ''}"
					>{t('nav.server')}{#if server.info?.available}<span class="flex items-center gap-1.5 text-xs text-fg"
							><span class="size-1.5 rounded-full bg-accent"></span>{t('nav.updateAvailable')}</span
						>{/if}</a
				>
				<a
					href="/backups"
					class="rounded-lg px-2 py-1.5 text-muted hover:bg-hover hover:text-fg {page.url.pathname === '/backups'
						? 'bg-selected text-fg'
						: ''}">{t('nav.backups')}</a
				>
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
