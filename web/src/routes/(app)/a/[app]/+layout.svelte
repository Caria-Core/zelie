<script lang="ts">
	import { ArrowUpRight, RotateCw, Rocket } from '@lucide/svelte';
	import { untrack } from 'svelte';
	import { page } from '$app/state';
	import { t } from '$lib/i18n';
	import { busy, current, deploy, load, restart } from '$lib/current.svelte';
	import { messageOf } from '$lib/errors';
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

	const tabs = $derived([
		{ href: `/a/${id}`, label: t('app.tab.deployments') },
		{ href: `/a/${id}/logs`, label: t('app.tab.logs') },
		{ href: `/a/${id}/env`, label: t('app.tab.env') },
		{ href: `/a/${id}/settings`, label: t('app.tab.settings') }
	]);
</script>

{#if current.missing}
	<p class="text-muted">404</p>
{:else if current.app && current.app.id === id}
	{@const a = current.app}
	<div class="flex flex-col gap-6">
		<header class="flex flex-wrap items-start justify-between gap-4">
			<div class="flex min-w-0 items-center gap-4">
				<AppIcon source={a.source} size="lg" />
				<div class="min-w-0">
					<h1 class="truncate text-[22px] tracking-tight">{a.id}</h1>
					<p class="flex flex-wrap items-center gap-x-2 text-sm text-muted">
						<StateDot state={a.state} label />
						<span>·</span>
						<span class="font-mono">{a.source === 'github' ? `${a.repo}@${a.branch}` : a.image}</span>
						{#if a.domain}
							<span>·</span>
							<a href="https://{a.domain}" target="_blank" rel="noopener noreferrer" class="inline-flex items-center gap-0.5 hover:text-fg"
								>{a.domain}<ArrowUpRight size={14} /></a
							>
						{/if}
					</p>
				</div>
			</div>
			<div class="flex items-center gap-2">
				{#if a.state !== 'none'}
					<Button kind="secondary" onclick={() => run(restart)} busy={starting || busy()} title={t('app.restartHint')}>
						<RotateCw size={16} strokeWidth={1.75} />{t('app.restart')}
					</Button>
				{/if}
				<Button onclick={() => run(deploy)} busy={starting || busy()}>
					<Rocket size={16} strokeWidth={1.75} />{busy() ? t('app.deploying') : t('app.deploy')}
				</Button>
			</div>
		</header>
		<ErrorText message={error} />
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
