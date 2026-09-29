<script lang="ts">
	import { ArrowUpRight, CircleArrowUp, Clock, Cpu, Ellipsis, MemoryStick, Play, RotateCw, Rocket, Square } from '@lucide/svelte';
	import { untrack } from 'svelte';
	import { page } from '$app/state';
	import { api } from '$lib/api';
	import { engineLabel, shownState } from '$lib/apps.svelte';
	import { ask } from '$lib/ask.svelte';
	import { say, t } from '$lib/i18n';
	import { busy, current, deploy, load, restart, start, stop, update } from '$lib/current.svelte';
	import { messageOf } from '$lib/errors';
	import { megabytes, type Usage } from '$lib/host.svelte';
	import AppIcon from '$lib/ui/AppIcon.svelte';
	import Button from '$lib/ui/Button.svelte';
	import ErrorText from '$lib/ui/ErrorText.svelte';
	import StateDot from '$lib/ui/StateDot.svelte';

	let { children } = $props();
	const id = $derived(page.params.app ?? '');
	let error = $state('');
	let starting = $state(false);

	$effect(() => {
		const app = id;
		current.app = null;
		load(app);
		// Faster while a deployment moves, slower otherwise. Untracked:
		// reading the app here would rerun this effect on every load.
		let timer: ReturnType<typeof setTimeout>;
		const tick = () => {
			timer = setTimeout(
				async () => {
					if (document.visibilityState === 'visible') await load(app);
					tick();
				},
				untrack(busy) ? 2000 : 10000
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
	const cores = (c: number) => (Number.isInteger(c) ? String(c) : c.toFixed(2).replace(/0$/, ''));

	// With pushes deploying on their own, deploying by hand is rare and goes
	// in a menu.
	let more = $state(false);
	let menu = $state<HTMLElement>();

	async function askStop() {
		const ok = await ask({ title: t('app.stopConfirm', { app: id }), text: t('app.stopConfirmText'), action: t('app.stop'), danger: true });
		if (ok) run(stop);
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
		current.app?.engine
			? [
					{ href: `/a/${id}`, label: t('db.tab.overview') },
					{ href: `/a/${id}/data`, label: t('viewer.tab') },
					{ href: `/a/${id}/logs`, label: t('app.tab.logs') },
					{ href: `/a/${id}/backups`, label: t('backups.tab') },
					{ href: `/a/${id}/storage`, label: t('app.tab.storage') },
					{ href: `/a/${id}/settings`, label: t('app.tab.settings') }
				]
			: [
					{ href: `/a/${id}`, label: t('app.tab.deployments') },
					{ href: `/a/${id}/logs`, label: t('app.tab.logs') },
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

{#if current.missing}
	<p class="text-muted">404</p>
{:else if current.app && current.app.id === id}
	{@const a = current.app}
	<div class="flex flex-col gap-6">
		<header class="flex flex-wrap items-start justify-between gap-4">
			<div class="flex min-w-0 items-center gap-4">
				<AppIcon source={a.source} database={!!a.engine} size="lg" />
				<div class="min-w-0">
					<h1 class="truncate text-[22px] tracking-tight">{a.id}</h1>
					<p class="flex flex-wrap items-center gap-x-2 text-sm text-muted">
						<StateDot state={shownState(a)} label />
						<span>·</span>
						{#if a.engine}
							<span>{engineLabel[a.engine]} {a.engine_version}</span>
							{#if a.upgrade_to}
								<a href="/a/{a.id}/settings" class="text-fg underline decoration-line underline-offset-2 hover:decoration-fg"
									>{t('db.upgradeAvailable', { version: a.upgrade_to })}</a
								>
							{/if}
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
							<span class="inline-flex items-center gap-1.5"
								><MemoryStick size={15} strokeWidth={1.75} />{a.memory_mb ? t('app.usageMemory', { used: mb, limit: megabytes(a.memory_mb) }) : mb}</span
							>
							{#if u.cpu !== undefined}
								<span class="inline-flex items-center gap-1.5"
									><Cpu size={15} strokeWidth={1.75} />{u.cpu < 0.01
										? t('app.usageIdle')
										: a.cpus
											? t('app.usageCpu', { used: cores(u.cpu), limit: cores(a.cpus) })
											: t('app.usageCpuOnly', { used: cores(u.cpu) })}</span
								>
							{/if}
						</p>
					{/if}
				</div>
			</div>
			<div class="flex items-center gap-2">
				{#if a.stopped}
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
				{#if !a.engine && a.source === 'github' && a.auto_deploy}
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
		{:else if a.stopped}
			<p class="text-sm text-muted">{t('app.stoppedNote')}</p>
		{/if}
		{#if a.update && !busy()}
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
		{@render children()}
	</div>
{/if}
