<script lang="ts">
	import { Search, X } from '@lucide/svelte';
	import { untrack } from 'svelte';
	import { api } from '$lib/api';
	import { messageOf } from '$lib/errors';
	import { t } from '$lib/i18n';
	import { stamp, type ChatMessage } from '$lib/players';
	import { view } from '$lib/players.svelte';
	import Button from './Button.svelte';
	import ErrorText from './ErrorText.svelte';

	// player fixes the list to one player, as on the player drawer.
	let { app, player = '', compact = false }: { app: string; player?: string; compact?: boolean } = $props();
	const size = $derived(compact ? 20 : 100);
	let channel = $state<'' | 'global' | 'team'>('');
	let search = $state('');
	let who = $state<{ id: string; name: string } | null>(null);
	let messages = $state<ChatMessage[]>([]);
	let more = $state(false);
	let loaded = $state(false);
	let busy = $state(false);
	let error = $state('');
	let seq = 0;

	async function load(reset: boolean) {
		const mine = ++seq;
		busy = true;
		error = '';
		try {
			const q = new URLSearchParams({ player: player || who?.id || '', channel, search: search.trim(), limit: String(size) });
			if (!reset && messages.length) q.set('before', String(messages[messages.length - 1].id));
			const d = await api<{ messages: ChatMessage[] }>('GET', `/games/${encodeURIComponent(app)}/chat?${q}`);
			if (mine !== seq) return;
			messages = reset ? d.messages : [...messages, ...d.messages];
			more = d.messages.length >= size;
			loaded = true;
		} catch (err) {
			if (mine === seq) error = messageOf(err);
		} finally {
			if (mine === seq) busy = false;
		}
	}

	$effect(() => {
		app;
		player;
		channel;
		search;
		who;
		view.tick;
		const wait = setTimeout(() => untrack(() => load(true)), 250);
		return () => clearTimeout(wait);
	});

	const channels = ['', 'global', 'team'] as const;
</script>

<div class="flex flex-col gap-4">
	{#if !compact}
		<p class="text-sm text-muted">{t('players.chatLead')}</p>
		<div class="flex flex-col gap-3 sm:flex-row sm:items-center">
			<div class="inline-flex w-fit rounded-full bg-selected p-1 text-sm" role="group" aria-label={t('players.channel')}>
				{#each channels as c (c)}
					<button class="rounded-full px-3.5 py-1 transition {channel === c ? 'bg-bg shadow-sm' : 'text-muted hover:text-fg'}" aria-pressed={channel === c} onclick={() => (channel = c)}
						>{c === '' ? t('players.channelAll') : c === 'global' ? t('players.channelGlobal') : t('players.channelTeam')}</button
					>
				{/each}
			</div>
			<label class="relative block sm:ml-auto sm:w-64">
				<span class="sr-only">{t('players.chatSearch')}</span>
				<Search size={16} strokeWidth={1.75} class="pointer-events-none absolute top-3 left-3.5 text-muted" />
				<input bind:value={search} type="search" autocomplete="off" placeholder={t('players.chatSearch')} class="h-10 w-full rounded-xl border border-line bg-bg pr-3.5 pl-10 text-[15px] outline-none transition focus:border-muted" />
			</label>
		</div>
		{#if who}
			<p class="flex items-center gap-2 text-sm">
				<span class="inline-flex items-center gap-1 rounded-full bg-selected py-0.5 pr-1 pl-3">{who.name}<button class="rounded-full p-1 text-muted hover:text-fg" aria-label={t('players.clearFilter')} onclick={() => (who = null)}><X size={14} /></button></span>
			</p>
		{/if}
	{/if}
	<ErrorText message={error} />
	{#if loaded && messages.length === 0}
		<p class="rounded-2xl border border-line px-4 py-6 text-center text-sm text-muted">{t('players.noChat')}</p>
	{:else if messages.length}
		<ul class="flex flex-col divide-y divide-line rounded-2xl border border-line">
			{#each messages as m (m.id)}
				<li class="flex flex-col gap-0.5 px-4 py-2.5 {m.channel === 'team' ? 'border-l-2 border-l-muted bg-panel' : ''}">
					<div class="flex flex-wrap items-baseline gap-x-2 text-sm">
						{#if player}
							<span class="font-medium">{m.name}</span>
						{:else}
							<button class="font-medium underline decoration-line underline-offset-2 hover:decoration-fg" title={t('players.filterBy', { name: m.name })} onclick={() => (who = { id: m.player_id, name: m.name })}>{m.name}</button>
						{/if}
						{#if m.channel === 'team'}<span class="rounded-full bg-selected px-2 py-0.5 text-xs">{t('players.channelTeam')}</span>{/if}
						<span class="text-xs text-muted">{stamp(m.at)}</span>
					</div>
					<p class="text-[15px] break-words">{m.text}</p>
				</li>
			{/each}
		</ul>
		{#if more}<div><Button kind="secondary" {busy} onclick={() => load(false)}>{t('players.loadOlder')}</Button></div>{/if}
	{/if}
</div>
