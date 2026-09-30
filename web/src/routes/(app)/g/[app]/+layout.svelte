<script lang="ts">
	import { Check, Copy, CopyPlus, Cpu, Download, Ellipsis, HardDrive, MemoryStick, Play, RotateCw, Skull, Square } from '@lucide/svelte';
	import { untrack } from 'svelte';
	import { page } from '$app/state';
	import { api } from '$lib/api';
	import { ask } from '$lib/ask.svelte';
	import { messageOf } from '$lib/errors';
	import { address, defaultPort, game, gameState, loadGame, power, updateSteam } from '$lib/games.svelte';
	import { megabytes, type Usage } from '$lib/host.svelte';
	import { say, t } from '$lib/i18n';
	import { loadClass } from '$lib/load';
	import AppIcon from '$lib/ui/AppIcon.svelte';
	import Button from '$lib/ui/Button.svelte';
	import CloneDialog from '$lib/ui/CloneDialog.svelte';
	import ErrorText from '$lib/ui/ErrorText.svelte';
	import StateDot from '$lib/ui/StateDot.svelte';

	let { children } = $props();
	const id = $derived(page.params.app ?? '');
	let error = $state('');
	let acting = $state(false);
	let copied = $state(false);
	let more = $state(false);
	let cloning = $state(false);
	let menu = $state<HTMLElement>();

	const phase = $derived(game.info ? gameState(game.info) : '');
	const moving = $derived(phase === 'starting' || phase === 'stopping' || phase === 'installing');

	$effect(() => {
		const server = id;
		game.info = null;
		game.missing = false;
		game.gone = '';
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

	// What the server uses, asked for while it runs and shown on every tab.
	let usage = $state<Usage | null>(null);
	const live = $derived(phase === 'starting' || phase === 'running' || phase === 'stopping');
	$effect(() => {
		const server = id;
		usage = null;
		if (!live) return;
		let stopped = false;
		let timer: ReturnType<typeof setTimeout>;
		const tick = async () => {
			if (stopped) return;
			if (document.visibilityState === 'visible') {
				const u = await api<Usage>('GET', `/apps/${encodeURIComponent(server)}/usage`).catch(() => null);
				if (!stopped) usage = u;
			}
			timer = setTimeout(tick, 3000);
		};
		tick();
		return () => {
			stopped = true;
			clearTimeout(timer);
		};
	});
	const cores = (c: number) => (Number.isInteger(c) ? String(c) : c.toFixed(2).replace(/0$/, ''));

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
		{ href: `/g/${id}/backups`, label: t('game.tab.backups') },
		{ href: `/g/${id}/schedules`, label: t('game.tab.schedules') },
		{ href: `/g/${id}/metrics`, label: t('game.tab.metrics') },
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
	{#if game.gone}
		<div class="flex flex-col gap-3">
			<p role="alert" class="rounded-xl border border-danger/30 px-4 py-3 text-sm text-danger">{game.gone}</p>
			<a href="/" class="w-fit text-sm text-muted underline decoration-line underline-offset-2 hover:text-fg hover:decoration-fg">{t('nav.apps')}</a>
		</div>
	{:else}
		<p class="text-muted">404</p>
	{/if}
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
					<p class="mt-1.5 flex flex-wrap items-center gap-x-4 gap-y-1 text-sm text-muted tabular-nums" title={usage?.running ? t('app.usageTitle') : t('game.limitsTitle')}>
						{#if usage?.running && usage.memory_bytes !== undefined}
							<span class="inline-flex items-center gap-1.5 {loadClass(usage.memory_bytes / 2 ** 20, g.memory_mb)}"
								><MemoryStick size={15} strokeWidth={1.75} />{t('app.usageMemory', { used: megabytes(usage.memory_bytes / 2 ** 20), limit: megabytes(g.memory_mb) })}</span
							>
							<span class="inline-flex items-center gap-1.5 {loadClass(usage.cpu, g.cpus)}"
								><Cpu size={15} strokeWidth={1.75} />{t('app.usageCpu', { used: usage.cpu === undefined || usage.cpu < 0.01 ? '0' : cores(usage.cpu), limit: cores(g.cpus) })}</span
							>
						{:else}
							<span class="inline-flex items-center gap-1.5"><MemoryStick size={15} strokeWidth={1.75} />{t('game.limitMemory', { limit: megabytes(g.memory_mb) })}</span>
							<span class="inline-flex items-center gap-1.5"><Cpu size={15} strokeWidth={1.75} />{t('game.limitCpu', { limit: cores(g.cpus) })}</span>
						{/if}
						<span class="inline-flex items-center gap-1.5"><HardDrive size={15} strokeWidth={1.75} />{t('game.limitDisk', { limit: megabytes(g.disk_mb) })}</span>
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
								disabled={phase === 'installing' || !ready}
								onclick={() => ((more = false), (cloning = true))}
							>
								<span class="flex items-center gap-2 text-[15px] font-medium"><CopyPlus size={16} strokeWidth={1.75} />{t('game.clone')}</span>
								<span class="text-sm text-muted">{t('game.cloneHint')}</span>
							</button>
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
	<CloneDialog {id} running={up} bind:open={cloning} />
{/if}
