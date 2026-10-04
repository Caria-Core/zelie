<script lang="ts">
	import { t } from '$lib/i18n';
	import { date } from '$lib/format';
	import type { SteamInfo } from '$lib/players';

	let { banned = false, notes = 0, steam = null, age = true }: { banned?: boolean; notes?: number; steam?: SteamInfo | null; age?: boolean } = $props();
	const chip = 'rounded-full bg-selected px-2.5 py-0.5 text-xs whitespace-nowrap';
</script>

{#if banned}<span class="{chip} !bg-transparent text-danger ring-1 ring-danger/40">{t('players.banned')}</span>{/if}
{#if notes > 0}<span class={chip}>{t('players.notesCount', { n: notes })}</span>{/if}
{#if steam}
	{#if steam.vac_bans > 0}<span class="{chip} text-danger">{t('players.vac', { n: steam.vac_bans })}</span>{/if}
	{#if steam.game_bans > 0}<span class="{chip} text-danger">{t('players.gameBans', { n: steam.game_bans })}</span>{/if}
	{#if steam.community_banned}<span class="{chip} text-danger">{t('players.communityBan')}</span>{/if}
	{#if age && steam.account_created}<span class="{chip} text-muted">{t('players.accountCreated', { date: date(steam.account_created) })}</span>{/if}
{/if}
