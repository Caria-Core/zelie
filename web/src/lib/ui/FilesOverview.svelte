<script lang="ts">
	import { ArrowUpRight, LoaderCircle } from '@lucide/svelte';
	import { runtimeLabel, type AppDetail } from '$lib/apps.svelte';
	import { game, gameState } from '$lib/games.svelte';
	import { t } from '$lib/i18n';

	let { app }: { app: AppDetail } = $props();

	const g = $derived(game.info?.id === app.id ? game.info : null);
	const phase = $derived(g ? gameState(g) : '');
	// Installed, and never started: the first steps are still ahead.
	const fresh = $derived(g?.install.state === 'installed' && app.state === 'none' && phase === 'stopped');
</script>

<div class="flex max-w-2xl flex-col gap-8">
	{#if phase === 'installing'}
		<div class="flex items-start gap-3 rounded-xl border border-line px-4 py-3" role="status">
			<LoaderCircle size={18} class="mt-0.5 shrink-0 animate-spin text-muted" />
			<div class="min-w-0">
				<p class="text-sm font-medium">{t('console.installing')}</p>
				<p class="text-sm text-muted">
					{t('files.overview.installing')}
					<a href="/a/{app.id}/console" class="text-fg underline decoration-line underline-offset-2 hover:decoration-fg">{t('game.tab.console')}</a>
				</p>
			</div>
		</div>
	{:else if g?.install.state === 'failed'}
		<div class="rounded-xl border border-danger/30 px-4 py-3" role="status">
			<p class="text-sm font-medium text-danger">{t('console.installFailed')}</p>
			<p class="text-sm text-muted">
				{t('console.installFailedText')}
				<a href="/a/{app.id}/settings" class="text-fg underline decoration-line underline-offset-2 hover:decoration-fg">{t('app.tab.settings')}</a>
			</p>
		</div>
	{:else if fresh}
		<section class="flex flex-col gap-3 rounded-2xl border border-line p-5">
			<div>
				<h2 class="font-medium">{t('files.overview.nextTitle')}</h2>
				<p class="text-sm text-muted">{t('files.overview.nextLead')}</p>
			</div>
			<ol class="flex flex-col gap-2.5">
				<li class="flex gap-3 text-sm">
					<span class="grid size-6 shrink-0 place-items-center rounded-full bg-selected text-xs font-medium">1</span>
					<span class="pt-0.5 text-muted"
						>{t('files.overview.step1')}
						<a href="/a/{app.id}/files" class="text-fg underline decoration-line underline-offset-2 hover:decoration-fg">{t('game.tab.files')}</a></span
					>
				</li>
				<li class="flex gap-3 text-sm">
					<span class="grid size-6 shrink-0 place-items-center rounded-full bg-selected text-xs font-medium">2</span>
					<span class="pt-0.5 text-muted"
						>{t('files.overview.step2')}
						<a href="/a/{app.id}/startup" class="text-fg underline decoration-line underline-offset-2 hover:decoration-fg">{t('game.tab.startup')}</a></span
					>
				</li>
				<li class="flex gap-3 text-sm">
					<span class="grid size-6 shrink-0 place-items-center rounded-full bg-selected text-xs font-medium">3</span>
					<span class="pt-0.5 text-muted">{t('files.overview.step3')}</span>
				</li>
			</ol>
		</section>
	{/if}

	<dl class="flex flex-col divide-y divide-line rounded-2xl border border-line">
		<div class="flex flex-col gap-1 px-4 py-3 sm:flex-row sm:items-baseline sm:justify-between sm:gap-6">
			<dt class="text-sm text-muted">{t('files.overview.runtime')}</dt>
			<dd class="text-[15px] sm:text-right">
				{runtimeLabel[app.runtime ?? ''] ?? app.runtime}<span class="block font-mono text-xs break-all text-muted">{g?.image ?? app.image}</span>
			</dd>
		</div>
		<div class="flex flex-col gap-1 px-4 py-3 sm:flex-row sm:items-baseline sm:justify-between sm:gap-6">
			<dt class="text-sm text-muted">{t('files.overview.address')}</dt>
			<dd class="text-[15px] sm:text-right">
				{#if app.domain}
					<a href="https://{app.domain}" target="_blank" rel="noopener noreferrer" class="inline-flex items-center gap-0.5 hover:underline">{app.domain}<ArrowUpRight size={14} /></a>
				{:else}
					<span class="text-muted">{t('files.overview.noDomain')}</span>
					<a href="/a/{app.id}/settings" class="text-fg underline decoration-line underline-offset-2 hover:decoration-fg">{t('app.tab.settings')}</a>
				{/if}
			</dd>
		</div>
		<div class="flex flex-col gap-1 px-4 py-3 sm:flex-row sm:items-baseline sm:justify-between sm:gap-6">
			<dt class="text-sm text-muted">{t('files.overview.port')}</dt>
			<dd class="text-[15px] sm:text-right">
				<span class="font-mono">{app.port}</span><span class="block text-xs text-muted">{t('files.overview.portHint')}</span>
			</dd>
		</div>
		{#if g}
			<div class="flex flex-col gap-1.5 px-4 py-3">
				<dt class="text-sm text-muted">{t('startup.command')}</dt>
				<dd class="font-mono text-[13px] leading-relaxed break-all whitespace-pre-wrap">{g.startup_preview}</dd>
			</div>
		{/if}
	</dl>
</div>
