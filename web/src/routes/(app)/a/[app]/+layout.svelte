<script lang="ts">
	import { Activity, ArrowUpRight, CircleArrowUp, Clock, CopyPlus, Cpu, Ellipsis, MemoryStick, Play, RotateCw, Rocket, Square, TriangleAlert } from '@lucide/svelte';
	import { untrack } from 'svelte';
	import { page } from '$app/state';
	import { api } from '$lib/api';
	import { engineLabel, isFiles, runtimeLabel, shownState } from '$lib/apps.svelte';
	import { ask } from '$lib/ask.svelte';
	import { say, t } from '$lib/i18n';
	import { loadClass } from '$lib/load';
	import { busy, current, deploy, load, restart, start, stop, update } from '$lib/current.svelte';
	import { messageOf } from '$lib/errors';
	import { dateTime } from '$lib/format';
	import { game, gameState, loadGame, power } from '$lib/games.svelte';
	import { megabytes, type Usage } from '$lib/host.svelte';
	import AppIcon from '$lib/ui/AppIcon.svelte';
	import Button from '$lib/ui/Button.svelte';
	import CloneDialog from '$lib/ui/CloneDialog.svelte';
	import ErrorText from '$lib/ui/ErrorText.svelte';
	import OfflineNotice from '$lib/ui/OfflineNotice.svelte';
	import StateDot from '$lib/ui/StateDot.svelte';

	let { children } = $props();
	const id = $derived(page.params.app ?? '');
	let error = $state('');
	let starting = $state(false);

	// A files app is started from an egg, like a game server, and its state
	// and startup settings come from the game side of the API.
	const files = $derived(!!current.app && isFiles(current.app));
	const phase = $derived(files && game.info?.id === id ? gameState(game.info) : '');
	const moving = $derived(phase === 'starting' || phase === 'stopping' || phase === 'installing');

	async function refresh(app: string) {
		await load(app);
		if (current.app?.id === app && isFiles(current.app)) await loadGame(app);
	}

	$effect(() => {
		const app = id;
		current.app = null;
		current.offline = false;
		game.info = null;
		game.missing = false;
		game.gone = '';
		game.live = '';
		game.offline = false;
		refresh(app);
		// Faster while a deployment moves, slower otherwise. Untracked:
		// reading the app here would rerun this effect on every load.
		let timer: ReturnType<typeof setTimeout>;
		const tick = () => {
			timer = setTimeout(
				async () => {
					if (document.visibilityState === 'visible') await refresh(app);
					tick();
				},
				untrack(() => busy() || moving) ? 2000 : 10000
			);
		};
		tick();
		return () => clearTimeout(timer);
	});

	// What the app uses now, shown on every tab.
	$effect(() => {
		const app = id;
		current.usage = null;
		let stopped = false;
		const tick = async () => {
			if (stopped) return;
			if (document.visibilityState === 'visible') {
				const u = await api<Usage>('GET', `/apps/${encodeURIComponent(app)}/usage`).catch(() => null);
				if (!stopped) current.usage = u;
			}
			setTimeout(tick, 3000);
		};
		tick();
		return () => (stopped = true);
	});
	// How long the live version has run. A crash or a server restart starts
	// a new deployment, and so the count again; an update of Zelie does not.
	let now = $state(Date.now());
	$effect(() => {
		const timer = setInterval(() => (now = Date.now()), 1000);
		return () => clearInterval(timer);
	});
	function uptime(since: string): string {
		const s = Math.max(0, Math.floor((now - new Date(since).getTime()) / 1000));
		const [d, h, m] = [Math.floor(s / 86400), Math.floor((s % 86400) / 3600), Math.floor((s % 3600) / 60)];
		return t('app.uptime', { time: d ? `${d}d ${h}h` : h ? `${h}h ${m}m` : m ? `${m}m` : `${s}s` });
	}
	const reqs = (n: number) => (n >= 10 ? String(Math.round(n)) : String(Math.round(n * 10) / 10));
	const rate = (x: number) => `${(x * 100).toFixed(x < 0.01 ? 2 : 1)}%`;
	const cores = (c: number) => (Number.isInteger(c) ? String(c) : c.toFixed(2).replace(/0$/, ''));

	// With pushes deploying on their own, deploying by hand is rare and goes
	// in a menu.
	let more = $state(false);
	let cloning = $state(false);
	let menu = $state<HTMLElement>();

	async function askStop() {
		const ok = await ask({ title: t('app.stopConfirm', { app: id }), text: t('app.stopConfirmText'), action: t('app.stop'), danger: true });
		if (ok) run(stop);
	}

	async function powered(action: 'start' | 'stop' | 'restart') {
		starting = true;
		error = '';
		try {
			await power(id, action);
			await load(id);
		} catch (err) {
			error = messageOf(err);
		} finally {
			starting = false;
		}
	}

	async function askFilesStop() {
		const ok = await ask({ title: t('app.stopConfirm', { app: id }), text: t('files.stopConfirmText'), action: t('app.stop'), danger: true });
		if (ok) powered('stop');
	}

	async function askFilesRestart() {
		const ok = await ask({ title: t('app.restartConfirm', { app: id }), text: t('files.restartConfirmText'), action: t('app.restart') });
		if (ok) powered('restart');
	}

	async function askRestart() {
		const ok = await ask({ title: t('app.restartConfirm', { app: id }), text: t('app.restartConfirmText'), action: t('app.restart') });
		if (ok) run(restart);
	}

	async function askUpdate() {
		const a = current.app!;
		const ok = await ask({
			title: t('app.updateConfirm', { app: id }),
			text: a.engine ? t('app.updateDbText') : t('app.updateAppText'),
			action: t('app.update')
		});
		if (ok) run(update);
	}

	// What the update notice says: versions when the images give them.
	function updateLine(a: NonNullable<typeof current.app>): string {
		const u = a.update!;
		const name = a.engine ? `${engineLabel[a.engine]} ` : '';
		if (u.current && u.version) return name + t('app.updateVersions', { from: u.current, to: u.version });
		if (u.version) return name + t('app.updateTo', { to: u.version });
		return t('app.updateBuild', { image: a.image ?? '' });
	}

	async function run(fn: (id: string) => Promise<void>) {
		starting = true;
		error = '';
		try {
			await fn(id);
		} catch (err) {
			error = messageOf(err);
		} finally {
			starting = false;
		}
	}

	// A database has no deployments to speak of and variables Zelie sets.
	const tabs = $derived(
		files
			? [
					{ href: `/a/${id}`, label: t('files.tab.overview') },
					{ href: `/a/${id}/console`, label: t('game.tab.console') },
					{ href: `/a/${id}/files`, label: t('game.tab.files') },
					{ href: `/a/${id}/startup`, label: t('game.tab.startup') },
					{ href: `/a/${id}/env`, label: t('app.tab.env') },
					{ href: `/a/${id}/metrics`, label: t('app.tab.metrics') },
					{ href: `/a/${id}/storage`, label: t('app.tab.storage') },
					{ href: `/a/${id}/backups`, label: t('backups.tab') },
					{ href: `/a/${id}/settings`, label: t('app.tab.settings') }
				]
			: current.app?.engine
			? [
					{ href: `/a/${id}`, label: t('db.tab.overview') },
					{ href: `/a/${id}/data`, label: t('viewer.tab') },
					{ href: `/a/${id}/logs`, label: t('app.tab.logs') },
					{ href: `/a/${id}/metrics`, label: t('app.tab.metrics') },
					{ href: `/a/${id}/backups`, label: t('backups.tab') },
					{ href: `/a/${id}/storage`, label: t('app.tab.storage') },
					{ href: `/a/${id}/settings`, label: t('app.tab.settings') }
				]
			: [
					{ href: `/a/${id}`, label: t('app.tab.deployments') },
					{ href: `/a/${id}/logs`, label: t('app.tab.logs') },
					{ href: `/a/${id}/metrics`, label: t('app.tab.metrics') },
					{ href: `/a/${id}/env`, label: t('app.tab.env') },
					{ href: `/a/${id}/storage`, label: t('app.tab.storage') },
					{ href: `/a/${id}/backups`, label: t('backups.tab') },
					{ href: `/a/${id}/settings`, label: t('app.tab.settings') }
				]
	);
</script>

<svelte:window
	onclick={(e) => {
		if (more && menu && !menu.contains(e.target as Node)) more = false;
	}}
	onkeydown={(e) => {
		if (e.key === 'Escape') more = false;
	}}
/>

{#if current.offline || (files && game.offline)}<OfflineNotice />{/if}
{#if current.missing}
	{#if current.gone}
		<div class="flex flex-col gap-3">
			<p role="alert" class="rounded-xl border border-danger/30 px-4 py-3 text-sm text-danger">{current.gone}</p>
			<a href="/" class="w-fit text-sm text-muted underline decoration-line underline-offset-2 hover:text-fg hover:decoration-fg">{t('nav.apps')}</a>
		</div>
	{:else}
		<p class="text-muted">404</p>
	{/if}
{:else if current.app && current.app.id === id}
	{@const a = current.app}
	<div class="flex flex-col gap-6">
		<header class="flex flex-wrap items-start justify-between gap-4">
			<div class="flex min-w-0 items-center gap-4">
				<AppIcon source={a.source} database={!!a.engine} size="lg" />
				<div class="min-w-0">
					<h1 class="truncate text-[22px] tracking-tight">{a.id}</h1>
					<p class="flex flex-wrap items-center gap-x-2 text-sm text-muted">
						<StateDot state={files && phase ? phase : shownState(a)} label />
						<span>·</span>
						{#if a.engine}
							<span>{engineLabel[a.engine]} {a.engine_version}</span>
							{#if a.upgrade_to}
								<a href="/a/{a.id}/settings" class="text-fg underline decoration-line underline-offset-2 hover:decoration-fg"
									>{t('db.upgradeAvailable', { version: a.upgrade_to })}</a
								>
							{/if}
						{:else if files}
							<span>{runtimeLabel[a.runtime ?? ''] ?? a.runtime}</span>
						{:else}
							<span class="font-mono">{a.source === 'github' ? `${a.repo}@${a.branch}` : a.image}</span>
						{/if}
						{#if a.domain}
							<span>·</span>
							<a href="https://{a.domain}" target="_blank" rel="noopener noreferrer" class="inline-flex items-center gap-0.5 hover:text-fg"
								>{a.domain}<ArrowUpRight size={14} /></a
							>
						{/if}
					</p>
					{#if current.usage?.running && current.usage.memory_bytes !== undefined}
						{@const u = current.usage}
						{@const mb = megabytes((u.memory_bytes ?? 0) / 2 ** 20)}
						{@const since = a.deployments.find((d) => d.state === 'live')?.finished_at}
						<p class="mt-1.5 flex flex-wrap items-center gap-x-4 gap-y-1 text-sm text-muted tabular-nums" title={t('app.usageTitle')}>
							{#if since}
								<span class="inline-flex items-center gap-1.5" title={t('app.uptimeTitle')}><Clock size={15} strokeWidth={1.75} />{uptime(since)}</span>
							{/if}
							<span class="inline-flex items-center gap-1.5 {loadClass(u.memory_bytes === undefined ? undefined : u.memory_bytes / 2 ** 20, a.memory_mb)}"
								><MemoryStick size={15} strokeWidth={1.75} />{a.memory_mb ? t('app.usageMemory', { used: mb, limit: megabytes(a.memory_mb) }) : mb}</span
							>
							{#if u.cpu !== undefined}
								<span class="inline-flex items-center gap-1.5 {loadClass(u.cpu, a.cpus)}"
									><Cpu size={15} strokeWidth={1.75} />{u.cpu < 0.01
										? t('app.usageIdle')
										: a.cpus
											? t('app.usageCpu', { used: cores(u.cpu), limit: cores(a.cpus) })
											: t('app.usageCpuOnly', { used: cores(u.cpu) })}</span
								>
							{/if}
							{#if u.requests_per_min !== undefined}
								<a href="/a/{a.id}/metrics" class="inline-flex items-center gap-1.5 hover:text-fg" title={t('app.summaryTitle')}
									><Activity size={15} strokeWidth={1.75} />{t('app.summaryRequests', { count: reqs(u.requests_per_min) })}{#if u.error_rate}<span
											class="text-danger">&nbsp;· {t('app.summaryErrors', { rate: rate(u.error_rate) })}</span
										>{/if}</a
								>
							{/if}
							{#if u.crashes}
								<a href="/a/{a.id}/metrics" class="inline-flex items-center gap-1.5 text-warn"
									><TriangleAlert size={15} strokeWidth={1.75} />{t('app.summaryCrashes', { count: u.crashes })}</a
								>
							{/if}
						</p>
					{/if}
				</div>
			</div>
			<div class="flex items-center gap-2">
				{#if files}
					{#if phase === 'running' || phase === 'starting' || phase === 'stopping'}
						<Button kind="quiet" onclick={askFilesStop} busy={starting} disabled={phase === 'stopping'} title={t('app.stopHint')}>
							<Square size={14} strokeWidth={1.75} />{t('app.stop')}
						</Button>
						<Button kind="secondary" onclick={askFilesRestart} busy={starting} disabled={phase === 'stopping' || !!a.volume_full} title={a.volume_full ? say(a.volume_full) : t('files.restartHint')}>
							<RotateCw size={16} strokeWidth={1.75} />{t('app.restart')}
						</Button>
					{:else}
						<Button
							kind="secondary"
							onclick={() => powered('start')}
							busy={starting}
							disabled={!game.info || game.info.install.state !== 'installed' || moving || !!a.volume_full}
							title={a.volume_full ? say(a.volume_full) : game.info && game.info.install.state !== 'installed' ? t('game.startNotReady') : undefined}
						>
							<Play size={16} strokeWidth={1.75} />{t('app.start')}
						</Button>
					{/if}
				{:else if a.stopped || a.state === 'none'}
					<!-- Never started, or the first start failed: start deploys it. -->
					<Button kind="secondary" onclick={() => run(start)} busy={starting || busy()} disabled={!!a.volume_full} title={a.volume_full ? say(a.volume_full) : undefined}>
						<Play size={16} strokeWidth={1.75} />{t('app.start')}
					</Button>
				{:else if a.state !== 'none'}
					<Button kind="quiet" onclick={askStop} busy={starting} title={t('app.stopHint')}>
						<Square size={14} strokeWidth={1.75} />{t('app.stop')}
					</Button>
					<Button kind="secondary" onclick={askRestart} busy={starting || busy()} disabled={!!a.volume_full} title={a.volume_full ? say(a.volume_full) : t('app.restartHint')}>
						<RotateCw size={16} strokeWidth={1.75} />{t('app.restart')}
					</Button>
				{/if}
				{#if files}
					<!-- Nothing to deploy: it runs the files in its volume. -->
					<div class="relative" bind:this={menu}>
						<Button kind="secondary" class="!px-3" aria-label={t('app.more')} title={t('app.more')} aria-expanded={more} onclick={() => (more = !more)}>
							<Ellipsis size={18} strokeWidth={1.75} />
						</Button>
						{#if more}
							<div class="absolute top-12 right-0 z-20 w-[min(20rem,calc(100vw-2rem))] rounded-2xl border border-line bg-bg p-1.5 shadow-lg">
								<button
									class="flex w-full flex-col items-start gap-1 rounded-xl px-3 py-2.5 text-left transition hover:bg-hover disabled:opacity-50"
									disabled={phase === 'installing' || game.info?.install.state !== 'installed'}
									onclick={() => ((more = false), (cloning = true))}
								>
									<span class="flex items-center gap-2 text-[15px] font-medium"><CopyPlus size={16} strokeWidth={1.75} />{t('game.clone')}</span>
									<span class="text-sm text-muted">{t('game.cloneHint')}</span>
								</button>
							</div>
						{/if}
					</div>
				{:else if !a.engine && a.source === 'github' && a.auto_deploy}
					<div class="relative" bind:this={menu}>
						<Button kind="secondary" class="!px-3" aria-label={t('app.more')} title={t('app.more')} aria-expanded={more} onclick={() => (more = !more)}>
							<Ellipsis size={18} strokeWidth={1.75} />
						</Button>
						{#if more}
							<div class="absolute top-12 right-0 z-20 w-[min(20rem,calc(100vw-2rem))] rounded-2xl border border-line bg-bg p-1.5 shadow-lg">
								<button
									class="flex w-full flex-col items-start gap-1 rounded-xl px-3 py-2.5 text-left transition hover:bg-hover disabled:opacity-50"
									disabled={starting || busy() || !!a.volume_full}
									onclick={() => ((more = false), run(deploy))}
								>
									<span class="flex items-center gap-2 text-[15px] font-medium"
										><Rocket size={16} strokeWidth={1.75} />{busy() ? t('app.deploying') : t('app.deployLatest')}</span
									>
									<span class="text-sm text-muted">{a.volume_full ? say(a.volume_full) : t('app.deployLatestHint')}</span>
								</button>
							</div>
						{/if}
					</div>
				{:else if !a.engine}
					<Button onclick={() => run(deploy)} busy={starting || busy()} disabled={!!a.volume_full} title={a.volume_full ? say(a.volume_full) : undefined}>
						<Rocket size={16} strokeWidth={1.75} />{busy() ? t('app.deploying') : t('app.deploy')}
					</Button>
				{/if}
			</div>
		</header>
		<ErrorText message={error} />
		{#if a.volume_full}
			<p class="rounded-xl border border-danger/30 px-4 py-3 text-sm text-danger">
				{t('app.volumeFull', { why: say(a.volume_full) })}
				<a href="/a/{a.id}/storage" class="underline underline-offset-2">{t('app.tab.storage')}</a>
			</p>
		{:else if a.crashing}
			<p class="rounded-xl border border-danger/30 px-4 py-3 text-sm text-danger">{t('app.crashing', { why: say(a.crashing) })}</p>
		{:else if a.stopped && a.stopped_for}
			<p class="rounded-xl border border-danger/30 px-4 py-3 text-sm text-danger">
				{t('app.stoppedFor', { why: say(a.stopped_for) })}
				<a href="/a/{a.id}/storage" class="underline underline-offset-2">{t('app.tab.storage')}</a>
			</p>
		{:else if a.stopped}
			<p class="text-sm text-muted">{files ? t('files.stoppedNote') : t('app.stoppedNote')}</p>
		{/if}
		{#if !a.stopped && a.layer_grace}
			<p class="rounded-xl border border-warn/40 bg-warn/5 px-4 py-3 text-sm text-warn">
				{t('app.layerGrace', { when: dateTime(a.layer_grace.until), why: say(a.layer_grace.why) })}
				<a href="/a/{a.id}/storage" class="underline underline-offset-2">{t('app.tab.storage')}</a>
			</p>
		{/if}
		{#if a.update && !busy() && !files}
			<div class="flex flex-wrap items-center justify-between gap-x-6 gap-y-3 rounded-xl border border-line px-4 py-3">
				<div class="flex min-w-0 flex-1 basis-72 items-start gap-3">
					<CircleArrowUp size={18} class="mt-0.5 shrink-0 text-ok" />
					<div class="min-w-0">
						<p class="text-sm"><span class="font-medium">{t('app.updateTitle')}</span> · {updateLine(a)}</p>
						<p class="mt-0.5 text-sm text-muted">{a.engine ? t('app.updateDbShort') : t('app.updateAppShort')}</p>
					</div>
				</div>
				<Button kind="secondary" onclick={askUpdate} busy={starting} disabled={!!a.volume_full || (!!a.engine && a.stopped)}>{t('app.update')}</Button>
			</div>
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
		<!-- The egg pages of a files app read what the game side of the API says. -->
		{#if !files || (game.info && game.info.id === id)}
			{@render children()}
		{/if}
	</div>
	{#if files}
		<CloneDialog {id} files running={phase === 'running' || phase === 'starting'} bind:open={cloning} />
	{/if}
{/if}
