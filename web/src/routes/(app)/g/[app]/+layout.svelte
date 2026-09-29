<script lang="ts">
	import { Check, Copy, Download, Ellipsis, Play, RotateCw, Skull, Square } from '@lucide/svelte';
	import { untrack } from 'svelte';
	import { page } from '$app/state';
	import { ask } from '$lib/ask.svelte';
	import { messageOf } from '$lib/errors';
	import { address, defaultPort, game, gameState, loadGame, power, updateSteam } from '$lib/games.svelte';
	import { say, t } from '$lib/i18n';
	import AppIcon from '$lib/ui/AppIcon.svelte';
	import Button from '$lib/ui/Button.svelte';
	import ErrorText from '$lib/ui/ErrorText.svelte';
	import StateDot from '$lib/ui/StateDot.svelte';

	let { children } = $props();
	const id = $derived(page.params.app ?? '');
	let error = $state('');
	let acting = $state(false);
	let copied = $state(false);
	let more = $state(false);
	let menu = $state<HTMLElement>();

	const phase = $derived(game.info ? gameState(game.info) : '');
	const moving = $derived(phase === 'starting' || phase === 'stopping' || phase === 'installing');

	$effect(() => {
		const server = id;
		game.info = null;
		game.missing = false;
		game.live = '';
		loadGame(server);
		// Faster while something is changing, slower otherwise.
		let timer: ReturnType<typeof setTimeout>;
		const tick = () => {
			timer = setTimeout(
				async () => {
					if (document.visibilityState === 'visible') await loadGame(server);
					tick();
				},
				untrack(() => moving) ? 2000 : 10000
			);
		};
		tick();
		return () => clearTimeout(timer);
	});

	async function run(action: 'start' | 'stop' | 'restart' | 'kill') {
		acting = true;
		error = '';
		try {
			await power(id, action);
		} catch (err) {
			error = messageOf(err);
		} finally {
			acting = false;
		}
	}

	async function confirmed(action: 'stop' | 'restart' | 'kill') {
		const ok = await ask({
			title: t(`game.${action}Confirm`, { server: id }),
			text: t(`game.${action}ConfirmText`),
			action: t(`game.${action}`),
			danger: action !== 'restart'
		});
		if (ok) run(action);
	}

	async function updateNow() {
		const running = phase === 'running' || phase === 'starting';
		const ok = await ask({
			title: t('game.updateConfirm', { server: id }),
			text: t(running ? 'game.updateConfirmRunning' : 'game.updateConfirmStopped'),
			action: t('game.updateNow'),
			danger: false
		});
		if (!ok) return;
		acting = true;
		error = '';
		try {
			await updateSteam(id);
		} catch (err) {
			error = messageOf(err);
		} finally {
			acting = false;
		}
	}

	async function copy(text: string) {
		try {
			await navigator.clipboard.writeText(text);
			copied = true;
			setTimeout(() => (copied = false), 1500);
		} catch {
			// The address is on screen to select by hand.
		}
	}

	const tabs = $derived([
		{ href: `/g/${id}`, label: t('game.tab.console') },
		{ href: `/g/${id}/files`, label: t('game.tab.files') },
		{ href: `/g/${id}/startup`, label: t('game.tab.startup') },
		{ href: `/g/${id}/network`, label: t('game.tab.network') },
		{ href: `/g/${id}/settings`, label: t('game.tab.settings') }
	]);
</script>

<svelte:window
	onclick={(e) => {
		if (more && menu && !menu.contains(e.target as Node)) more = false;
	}}
	onkeydown={(e) => {
		if (e.key === 'Escape') more = false;
	}}
/>

{#if game.missing}
	<p class="text-muted">404</p>
{:else if game.info && game.info.id === id}
	{@const g = game.info}
	{@const port = defaultPort(g)}
	{@const up = phase === 'running' || phase === 'starting' || phase === 'stopping'}
	{@const ready = g.install.state === 'installed'}
	<div class="flex flex-col gap-6">
		<header class="flex flex-wrap items-start justify-between gap-4">
			<div class="flex min-w-0 items-center gap-4">
				<AppIcon game size="lg" />
				<div class="min-w-0">
					<h1 class="truncate text-[22px] tracking-tight">{g.id}</h1>
					<p class="flex flex-wrap items-center gap-x-2 text-sm text-muted">
						<StateDot state={phase} label />
						<span>·</span>
						<span>{g.egg}</span>
						{#if port}
							<span>·</span>
							<span class="inline-flex items-center gap-1">
								<span class="font-mono text-fg select-all">{address(port)}</span>
								<button
									class="rounded-md p-1 text-muted transition hover:bg-hover hover:text-fg"
									aria-label={t('game.copyAddress')}
									title={copied ? t('game.copied') : t('game.copyAddress')}
									onclick={() => copy(address(port))}
								>
									{#if copied}<Check size={14} />{:else}<Copy size={14} />{/if}
								</button>
							</span>
						{/if}
					</p>
				</div>
			</div>
			<div class="flex items-center gap-2">
				{#if up}
					<Button kind="quiet" onclick={() => confirmed('stop')} busy={acting} disabled={phase === 'stopping'}>
						<Square size={14} strokeWidth={1.75} />{t('game.stop')}
					</Button>
					<Button kind="secondary" onclick={() => confirmed('restart')} busy={acting} disabled={phase === 'stopping'}>
						<RotateCw size={16} strokeWidth={1.75} />{t('game.restart')}
					</Button>
				{:else}
					<Button kind="secondary" onclick={() => run('start')} busy={acting} disabled={!ready || moving} title={ready ? undefined : t('game.startNotReady')}>
						<Play size={16} strokeWidth={1.75} />{t('game.start')}
					</Button>
				{/if}
				<div class="relative" bind:this={menu}>
					<Button kind="secondary" class="!px-3" aria-label={t('app.more')} title={t('app.more')} aria-expanded={more} onclick={() => (more = !more)}>
						<Ellipsis size={18} strokeWidth={1.75} />
					</Button>
					{#if more}
						<div class="absolute top-12 right-0 z-20 w-[min(20rem,calc(100vw-2rem))] rounded-2xl border border-line bg-bg p-1.5 shadow-lg">
							<button
								class="flex w-full flex-col items-start gap-1 rounded-xl px-3 py-2.5 text-left transition hover:bg-hover disabled:opacity-50"
								disabled={acting || phase === 'installing' || phase === 'stopped'}
								onclick={() => ((more = false), confirmed('kill'))}
							>
								<span class="flex items-center gap-2 text-[15px] font-medium"><Skull size={16} strokeWidth={1.75} />{t('game.kill')}</span>
								<span class="text-sm text-muted">{t('game.killHint')}</span>
							</button>
						</div>
					{/if}
				</div>
			</div>
		</header>
		<ErrorText message={error} />
		{#if g.steam?.update_available && ready}
			<div class="flex flex-wrap items-center gap-x-3 gap-y-2 text-sm">
				<span class="rounded-full bg-selected px-2.5 py-0.5 text-xs">{t('game.updateAvailable')}</span>
				<span class="text-muted">{t('game.updateBuilds', { from: g.steam.installed_build, to: g.steam.latest_build })}</span>
				<Button kind="quiet" class="!h-8 !px-3 !text-sm" onclick={updateNow} busy={acting} disabled={moving}>
					<Download size={14} strokeWidth={1.75} />{t('game.updateNow')}
				</Button>
			</div>
		{/if}
		{#if g.crashing}
			<p class="rounded-xl border border-danger/30 px-4 py-3 text-sm text-danger">{t('game.crashing', { why: say(g.crashing) })}</p>
		{/if}
		<!-- The baseline is a shadow, not a border, so the active tab's underline
		     can sit on it without overflowing and bringing up a scroll bar. -->
		<nav class="flex gap-1 overflow-x-auto text-[15px] shadow-[inset_0_-1px_0_var(--line)] [scrollbar-width:none]">
			{#each tabs as tab (tab.href)}
				<a
					href={tab.href}
					class="border-b-2 px-3 py-2 whitespace-nowrap transition {page.url.pathname === tab.href
						? 'border-fg text-fg'
						: 'border-transparent text-muted hover:text-fg'}"
					aria-current={page.url.pathname === tab.href ? 'page' : undefined}>{tab.label}</a
				>
			{/each}
		</nav>
		{@render children()}
	</div>
{/if}
