<script lang="ts">
	import { Ban, Copy, Ellipsis, LogOut, ShieldCheck, ShieldOff, StickyNote, Undo2, User, UserCheck, UserMinus } from '@lucide/svelte';
	import { page } from '$app/state';
	import { t } from '$lib/i18n';
	import { isSteamId } from '$lib/players';
	import { toggle, unbanPlayer, view } from '$lib/players.svelte';

	// The actions for one player. Op and whitelist are Minecraft's.
	let { id, name, banned, online, minecraft, profile = true }: { id: string; name: string; banned: boolean; online: boolean; minecraft: boolean; profile?: boolean } = $props();
	const app = $derived(page.params.app ?? '');
	let open = $state(false);
	let root = $state<HTMLElement>();
	const item = 'flex w-full items-center gap-2.5 rounded-xl px-3 py-2 text-left text-[15px] transition hover:bg-hover';

	const pick = (fn: () => void) => () => {
		open = false;
		fn();
	};
</script>

<svelte:window
	onclick={(e) => {
		if (open && root && !root.contains(e.target as Node)) open = false;
	}}
	onkeydown={(e) => {
		if (e.key === 'Escape') open = false;
	}}
/>

<div class="relative" bind:this={root}>
	<button
		class="rounded-lg p-2 text-muted transition hover:bg-hover hover:text-fg"
		aria-label={t('players.menu', { name })}
		aria-haspopup="menu"
		aria-expanded={open}
		onclick={() => (open = !open)}
	>
		<Ellipsis size={18} strokeWidth={1.75} />
	</button>
	{#if open}
		<div role="menu" class="absolute top-9 right-0 z-20 w-52 rounded-2xl border border-line bg-bg p-1.5 shadow-lg">
			{#if profile}
				<button role="menuitem" class={item} onclick={pick(() => (view.detail = id))}><User size={16} strokeWidth={1.75} />{t('players.openProfile')}</button>
			{/if}
			{#if online}
				<button role="menuitem" class={item} onclick={pick(() => (view.form = { kind: 'kick', id, name }))}><LogOut size={16} strokeWidth={1.75} />{t('players.kick')}</button>
			{/if}
			{#if banned}
				<button role="menuitem" class={item} onclick={pick(() => unbanPlayer(app, id, name))}><Undo2 size={16} strokeWidth={1.75} />{t('players.unban')}</button>
			{:else}
				<button role="menuitem" class="{item} text-danger" onclick={pick(() => (view.form = { kind: 'ban', id, name }))}><Ban size={16} strokeWidth={1.75} />{t('players.ban')}</button>
			{/if}
			<button role="menuitem" class={item} onclick={pick(() => (view.form = { kind: 'note', id, name }))}><StickyNote size={16} strokeWidth={1.75} />{t('players.addNote')}</button>
			{#if minecraft && !isSteamId(id)}
				<button role="menuitem" class={item} onclick={pick(() => toggle(app, id, 'op', true))}><ShieldCheck size={16} strokeWidth={1.75} />{t('players.op')}</button>
				<button role="menuitem" class={item} onclick={pick(() => toggle(app, id, 'op', false))}><ShieldOff size={16} strokeWidth={1.75} />{t('players.deop')}</button>
				<button role="menuitem" class={item} onclick={pick(() => toggle(app, id, 'whitelist', true))}><UserCheck size={16} strokeWidth={1.75} />{t('players.whitelistAdd')}</button>
				<button role="menuitem" class={item} onclick={pick(() => toggle(app, id, 'whitelist', false))}><UserMinus size={16} strokeWidth={1.75} />{t('players.whitelistRemove')}</button>
			{/if}
			<button role="menuitem" class={item} onclick={pick(() => navigator.clipboard?.writeText(id).catch(() => {}))}><Copy size={16} strokeWidth={1.75} />{t('players.copyId')}</button>
		</div>
	{/if}
</div>
