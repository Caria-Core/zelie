<script lang="ts">
	import { onMount } from 'svelte';
	import { page } from '$app/state';
	import { goto } from '$app/navigation';
	import { game } from '$lib/games.svelte';
	import { t } from '$lib/i18n';
	import { view } from '$lib/players.svelte';
	import ErrorText from '$lib/ui/ErrorText.svelte';
	import Lead from '$lib/ui/Lead.svelte';
	import PlayerDetail from '$lib/ui/PlayerDetail.svelte';
	import PlayerForm from '$lib/ui/PlayerForm.svelte';
	import PlayersBans from '$lib/ui/PlayersBans.svelte';
	import PlayersChat from '$lib/ui/PlayersChat.svelte';
	import PlayersHistory from '$lib/ui/PlayersHistory.svelte';
	import PlayersOnline from '$lib/ui/PlayersOnline.svelte';
	import PlayersReports from '$lib/ui/PlayersReports.svelte';

	const id = $derived(page.params.app ?? '');
	const g = $derived(game.info);
	const full = $derived(g?.players === 'full');
	const minecraft = $derived(g?.players_game === 'minecraft');

	const sections = ['online', 'history', 'chat', 'reports', 'bans'] as const;
	type Section = (typeof sections)[number];
	let section = $state<Section>('online');
	const shown = $derived(full ? sections : (['online'] as const));

	onMount(() => {
		view.detail = '';
		view.form = null;
		view.error = '';
		view.tick = 0;
		return () => {
			view.detail = '';
			view.form = null;
		};
	});

	// A game without player support has no tab, so a link to it goes back.
	$effect(() => {
		if (g && g.id === id && !g.players) goto(`/g/${id}`, { replaceState: true });
	});
</script>

{#if g?.players}
	<div class="flex max-w-3xl flex-col gap-6">
		<Lead title={t('players.title')} text={' ' + t(full ? 'players.lead' : 'players.leadList')} />
		{#if full}
			<div class="flex gap-1 overflow-x-auto rounded-full bg-selected p-1 text-sm [scrollbar-width:none]" role="tablist">
				{#each shown as s (s)}
					<button
						role="tab"
						aria-selected={section === s}
						class="rounded-full px-4 py-1.5 whitespace-nowrap transition {section === s ? 'bg-bg shadow-sm' : 'text-muted hover:text-fg'}"
						onclick={() => (section = s)}>{t(`players.section.${s}`)}</button
					>
				{/each}
			</div>
		{/if}
		<ErrorText message={view.error} />
		{#if section === 'online' || !full}
			<PlayersOnline app={id} {minecraft} />
		{:else if section === 'history'}
			<PlayersHistory app={id} {minecraft} />
		{:else if section === 'chat'}
			<PlayersChat app={id} />
		{:else if section === 'reports'}
			<PlayersReports app={id} />
		{:else}
			<PlayersBans app={id} />
		{/if}
	</div>
	<PlayerDetail app={id} {minecraft} />
	<PlayerForm />
{/if}
