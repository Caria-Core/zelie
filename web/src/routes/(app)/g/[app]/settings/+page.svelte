<script lang="ts">
	import { RotateCcw, Trash } from '@lucide/svelte';
	import { goto } from '$app/navigation';
	import { api } from '$lib/api';
	import { reload } from '$lib/apps.svelte';
	import { ask } from '$lib/ask.svelte';
	import { messageOf } from '$lib/errors';
	import { game, gameState, loadGame, setAutoUpdate } from '$lib/games.svelte';
	import { t } from '$lib/i18n';
	import Button from '$lib/ui/Button.svelte';
	import ErrorText from '$lib/ui/ErrorText.svelte';
	import Field from '$lib/ui/Field.svelte';

	const g = $derived(game.info!);
	const phase = $derived(gameState(g));
	const stopped = $derived(phase === 'stopped' || phase === 'crashed');

	let error = $state('');
	let busy = $state(false);
	let typed = $state('');

	async function reinstall() {
		if (!(await ask({ title: t('settings.reinstallConfirm', { server: g.id }), text: t('settings.reinstallConfirmText'), action: t('settings.reinstall') }))) return;
		busy = true;
		error = '';
		try {
			await api('POST', `/games/${encodeURIComponent(g.id)}/reinstall`);
			await Promise.all([loadGame(g.id), reload()]);
			await goto(`/g/${g.id}`);
		} catch (err) {
			error = messageOf(err);
		} finally {
			busy = false;
		}
	}

	async function autoUpdate(on: boolean) {
		busy = true;
		error = '';
		try {
			await setAutoUpdate(g.id, on);
		} catch (err) {
			error = messageOf(err);
		} finally {
			busy = false;
		}
	}

	async function remove() {
		if (!(await ask({ title: t('settings.deleteServerConfirm', { id: g.id }), text: t('settings.deleteServerConfirmText'), action: t('settings.delete'), danger: true }))) return;
		busy = true;
		error = '';
		try {
			await api('DELETE', `/apps/${encodeURIComponent(g.id)}`);
			await reload();
			await goto('/');
		} catch (err) {
			error = messageOf(err);
			busy = false;
		}
	}
</script>

<div class="flex max-w-xl flex-col gap-10">
	<section class="flex flex-col gap-3">
		<div>
			<h2 class="font-medium">{t('settings.reinstallTitle')}</h2>
			<p class="text-sm text-muted">{t('settings.reinstallLead')}</p>
		</div>
		<Button kind="secondary" class="self-start" {busy} disabled={!stopped} onclick={reinstall}><RotateCcw size={16} />{t('settings.reinstall')}</Button>
		{#if !stopped}<p class="text-sm text-muted">{t('settings.reinstallStop')}</p>{/if}
	</section>

	{#if g.steam}
		<section class="flex flex-col gap-3">
			<div>
				<h2 class="font-medium">{t('settings.steamTitle')}</h2>
				<p class="text-sm text-muted">{t('settings.steamLead')}</p>
			</div>
			<label class="flex items-start gap-2.5 text-[15px]">
				<input
					type="checkbox"
					class="mt-1 size-4 accent-[var(--fg)]"
					checked={g.steam.auto_update}
					disabled={busy}
					onchange={(e) => autoUpdate(e.currentTarget.checked)}
				/>
				<span>{t('settings.steamAuto')}<span class="block text-sm text-muted">{t('settings.steamAutoHint')}</span></span>
			</label>
			<p class="text-sm text-muted">
				{g.steam.installed_build ? t('settings.steamInstalled', { build: g.steam.installed_build }) : t('settings.steamNoBuild')}
				{#if g.steam.latest_build}{' ' + t('settings.steamLatest', { build: g.steam.latest_build })}{/if}
			</p>
		</section>
	{/if}

	<section class="flex flex-col gap-4 rounded-2xl border border-danger/30 p-5">
		<div>
			<h2 class="font-medium">{t('settings.deleteServerTitle')}</h2>
			<p class="text-sm text-muted">{t('settings.deleteServerLead')}</p>
		</div>
		<Field label={t('settings.typeName', { id: g.id })} autocomplete="off" autocapitalize="off" spellcheck="false" bind:value={typed} />
		<Button kind="secondary" class="self-start hover:!text-danger" {busy} disabled={typed.trim() !== g.id} onclick={remove}
			><Trash size={16} />{t('settings.delete')}</Button
		>
	</section>
	<ErrorText message={error} />
</div>
