<script lang="ts">
	import { Search } from '@lucide/svelte';
	import { untrack } from 'svelte';
	import { api } from '$lib/api';
	import { ago } from '$lib/format';
	import { messageOf } from '$lib/errors';
	import { t } from '$lib/i18n';
	import { span, stamp, type Player, type PlayerList } from '$lib/players';
	import { view } from '$lib/players.svelte';
	import Button from './Button.svelte';
	import ErrorText from './ErrorText.svelte';
	import PlayerBadges from './PlayerBadges.svelte';
	import PlayerMenu from './PlayerMenu.svelte';
	import PlayerName from './PlayerName.svelte';
	import StateDot from './StateDot.svelte';

	let { app, minecraft }: { app: string; minecraft: boolean } = $props();
	const size = 50;
	let search = $state('');
	let players = $state<Player[]>([]);
	let total = $state(0);
	let loaded = $state(false);
	let busy = $state(false);
	let error = $state('');
	let seq = 0;

	async function load(reset: boolean) {
		const mine = ++seq;
		busy = true;
		error = '';
		const offset = reset ? 0 : players.length;
		try {
			const q = new URLSearchParams({ search: search.trim(), limit: String(size), offset: String(offset) });
			const d = await api<PlayerList>('GET', `/games/${encodeURIComponent(app)}/players?${q}`);
			if (mine !== seq) return;
			// Someone who joined since the last page moved to the top and pushed
			// the rest down, so the next page starts with a row already shown.
			const shown = new Set(players.map((p) => p.id));
			players = reset ? d.players : [...players, ...d.players.filter((p) => !shown.has(p.id))];
			total = d.total;
			loaded = true;
		} catch (err) {
			if (mine === seq) error = messageOf(err);
		} finally {
			if (mine === seq) busy = false;
		}
	}

	// Typing waits a moment, so each key does not ask the server.
	$effect(() => {
		app;
		search;
		view.tick;
		const wait = setTimeout(() => untrack(() => load(true)), 250);
		return () => clearTimeout(wait);
	});
</script>

<div class="flex flex-col gap-4">
	<div class="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
		<p class="text-sm text-muted">{t('players.historyLead')}</p>
		<label class="relative block sm:w-64">
			<span class="sr-only">{t('players.search')}</span>
			<Search size={16} strokeWidth={1.75} class="pointer-events-none absolute top-3 left-3.5 text-muted" />
			<input bind:value={search} type="search" autocomplete="off" placeholder={t('players.search')} class="h-10 w-full rounded-xl border border-line bg-bg pr-3.5 pl-10 text-[15px] outline-none transition focus:border-muted" />
		</label>
	</div>
	<ErrorText message={error} />
	{#if loaded && players.length === 0}
		<p class="rounded-2xl border border-line px-4 py-6 text-center text-sm text-muted">{search.trim() ? t('players.noMatch') : t('players.noHistory')}</p>
	{:else if players.length}
		<ul class="flex flex-col divide-y divide-line rounded-2xl border border-line">
			{#each players as p (p.id)}
				<li class="flex flex-col gap-2 px-4 py-3">
					<div class="flex items-center justify-between gap-3">
						<button class="flex min-w-0 items-center gap-2.5 text-left" onclick={() => (view.detail = p.id)}>
							<PlayerName name={p.name} steam={p.steam} />
							{#if p.online}<StateDot state="running" />{/if}
						</button>
						<PlayerMenu id={p.id} name={p.name} banned={p.banned} online={p.online} {minecraft} />
					</div>
					<div class="flex flex-wrap items-center gap-x-4 gap-y-1 text-sm text-muted">
						<span title={stamp(p.last_seen)}>{p.online ? t('players.onlineNow') : t('players.lastSeen', { when: ago(p.last_seen) })}</span>
						<span title={stamp(p.first_seen)}>{t('players.firstSeen', { when: ago(p.first_seen) })}</span>
						<span class="tabular-nums">{t('players.playTime', { time: span(p.play_seconds) })}</span>
					</div>
					{#if p.banned || p.notes}
						<div class="flex flex-wrap gap-1.5"><PlayerBadges banned={p.banned} notes={p.notes} steam={p.steam} age={false} /></div>
					{/if}
				</li>
			{/each}
		</ul>
		{#if players.length < total}
			<div><Button kind="secondary" {busy} onclick={() => load(false)}>{t('players.loadMore')}</Button></div>
		{/if}
	{/if}
</div>
