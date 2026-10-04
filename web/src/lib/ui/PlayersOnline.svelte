<script lang="ts">
	import { RefreshCw } from '@lucide/svelte';
	import { untrack } from 'svelte';
	import { api } from '$lib/api';
	import { messageOf } from '$lib/errors';
	import { say, t } from '$lib/i18n';
	import { poll, span, type Online } from '$lib/players';
	import { view } from '$lib/players.svelte';
	import CopyId from './CopyId.svelte';
	import ErrorText from './ErrorText.svelte';
	import IpText from './IpText.svelte';
	import IpToggle from './IpToggle.svelte';
	import PlayerBadges from './PlayerBadges.svelte';
	import PlayerMenu from './PlayerMenu.svelte';
	import PlayerName from './PlayerName.svelte';

	let { app, minecraft }: { app: string; minecraft: boolean } = $props();
	let data = $state<Online | null>(null);
	let error = $state('');

	async function load() {
		try {
			const d = await api<Online>('GET', `/games/${encodeURIComponent(app)}/players/online`);
			data = d;
			error = '';
		} catch (err) {
			error = messageOf(err);
		}
	}

	// Every 10 seconds while the page is on screen, and again after an action.
	$effect(() => {
		app;
		return untrack(() => poll(load, 10000));
	});
	let first = true;
	$effect(() => {
		view.tick;
		if (first) return void (first = false);
		untrack(load);
	});
</script>

<div class="flex flex-col gap-4">
	<div class="flex flex-wrap items-center justify-between gap-2">
		<p class="text-sm text-muted">
			{#if data?.running && !data.error}{t('players.onlineCount', { n: data.players.length })}{:else}{t('players.onlineLead')}{/if}
		</p>
		<IpToggle />
	</div>
	<ErrorText message={error} />
	{#if !data}
		<p class="flex items-center gap-2 text-sm text-muted"><RefreshCw size={14} class="animate-spin" />{t('players.loading')}</p>
	{:else if !data.running}
		<p class="rounded-2xl border border-line px-4 py-6 text-center text-sm text-muted">{t('players.stopped')}</p>
	{:else if data.error}
		<p class="rounded-2xl border border-line px-4 py-6 text-center text-sm text-muted" role="status">{t('players.askFailed')} <span class="break-words">{say({ code: data.error_code ?? '', text: data.error })}</span></p>
	{:else if data.players.length === 0}
		<p class="rounded-2xl border border-line px-4 py-6 text-center text-sm text-muted">{t('players.nobody')}</p>
	{:else}
		<ul class="flex flex-col divide-y divide-line rounded-2xl border border-line">
			{#each data.players as p (p.id)}
				<li class="flex flex-col gap-2 px-4 py-3">
					<div class="flex items-center justify-between gap-3">
						<button class="min-w-0 text-left" onclick={() => (view.detail = p.id)}><PlayerName name={p.name} steam={p.steam} /></button>
						<PlayerMenu id={p.id} name={p.name} banned={p.banned} online minecraft={minecraft} />
					</div>
					<div class="flex flex-wrap items-center gap-x-4 gap-y-1 text-sm text-muted">
						<CopyId text={p.id} />
						{#if p.ping !== null}<span class="tabular-nums">{t('players.ping', { ms: p.ping })}</span>{/if}
						{#if p.connected_seconds !== null}<span class="tabular-nums">{t('players.connected', { time: span(p.connected_seconds) })}</span>{/if}
						{#if p.ip}<IpText ip={p.ip} />{/if}
					</div>
					{#if p.banned || p.notes || p.steam}
						<div class="flex flex-wrap gap-1.5"><PlayerBadges banned={p.banned} notes={p.notes} steam={p.steam} /></div>
					{/if}
				</li>
			{/each}
		</ul>
		{#if data.source === 'query'}<p class="text-sm text-muted">{t('players.queryOnly')}</p>{/if}
	{/if}
</div>
