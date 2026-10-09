<script lang="ts">
	import { RotateCcw, RotateCw, Trash } from '@lucide/svelte';
	import { untrack } from 'svelte';
	import { goto } from '$app/navigation';
	import { api } from '$lib/api';
	import { reload } from '$lib/apps.svelte';
	import { ask } from '$lib/ask.svelte';
	import { messageOf } from '$lib/errors';
	import { game, gameState, loadGame, power, saveResources, setAutoUpdate } from '$lib/games.svelte';
	import { host, loadHost, megabytes } from '$lib/host.svelte';
	import { editHistory } from '$lib/history.svelte';
	import { t } from '$lib/i18n';
	import { sizeSteps } from '$lib/volumes';
	import Button from '$lib/ui/Button.svelte';
	import ErrorText from '$lib/ui/ErrorText.svelte';
	import Field from '$lib/ui/Field.svelte';
	import HostAccess from '$lib/ui/HostAccess.svelte';
	import Resources from '$lib/ui/Resources.svelte';
	import SaveBar from '$lib/ui/SaveBar.svelte';

	const g = $derived(game.info!);
	const phase = $derived(gameState(g));
	const stopped = $derived(phase === 'stopped' || phase === 'crashed');

	let error = $state('');
	let busy = $state(false);
	let typed = $state('');

	// The limits in the form. The page polls the server, so they are filled
	// once and again after a save.
	let memory = $state(0);
	let cpus = $state(0);
	let disk = $state(0);
	let sized = $state(false);
	let sizing = $state(false);
	let restarting = $state(false);
	let saved = $state(false);
	let sizeError = $state('');
	const up = $derived(phase === 'running' || phase === 'starting');
	const diskMB = $derived(host.info ? host.info.disk_bytes / 2 ** 20 : 102400);
	const resized = $derived(memory !== g.memory_mb || cpus !== g.cpus || disk !== g.disk_mb);

	const history = editHistory(
		() => ({ memory, cpus, disk }),
		(v) => ({ memory, cpus, disk } = v)
	);

	function fill() {
		[memory, cpus, disk] = [g.memory_mb, g.cpus, g.disk_mb];
		history.rebase();
	}

	$effect(() => {
		loadHost();
		g.id;
		untrack(() => {
			if (!sized) fill();
			sized = true;
		});
	});

	async function saveSizes() {
		sizing = true;
		sizeError = '';
		saved = false;
		try {
			await saveResources(g.id, { memory_mb: memory, cpus, disk_mb: disk });
			fill();
			saved = true;
		} catch (err) {
			sizeError = messageOf(err);
		} finally {
			sizing = false;
		}
	}

	async function restart() {
		if (!(await ask({ title: t('game.restartConfirm', { server: g.id }), text: t('game.restartConfirmText'), action: t('game.restart') }))) return;
		restarting = true;
		sizeError = '';
		try {
			await power(g.id, 'restart');
			saved = false;
		} catch (err) {
			sizeError = messageOf(err);
		} finally {
			restarting = false;
		}
	}

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
	<form
		class="flex flex-col gap-5"
		onsubmit={(e) => {
			e.preventDefault();
			if (resized && !sizing) saveSizes();
		}}
	>
		<SaveBar dirty={resized} busy={sizing} canUndo={history.canUndo} onsave={saveSizes} onundo={history.undo} onreset={history.revert} />
		<ErrorText message={sizeError} />
		{#if saved && !resized && up}
			<div class="flex flex-wrap items-center justify-between gap-3 rounded-xl border border-line px-4 py-3">
				<p class="text-sm">{t('settings.resourcesRestart')}</p>
				<Button type="button" kind="secondary" class="!h-8 !px-4" busy={restarting} onclick={restart}><RotateCw size={14} strokeWidth={1.75} />{t('startup.restart')}</Button>
			</div>
		{/if}
		<div>
			<h2 class="font-medium">{t('settings.resourcesTitle')}</h2>
			<p class="text-sm text-muted">{t('settings.gameResourcesLead')}</p>
		</div>
		{#if sized}
			<Resources bind:memory bind:cpus app={g.id} game />
			<div class="flex flex-col gap-1.5">
				<label for="disk" class="text-sm font-medium">{t('game.new.disk')}</label>
				<select id="disk" class="h-10 w-40 rounded-xl border border-line bg-bg px-3 text-[15px]" bind:value={disk}>
					{#each sizeSteps(diskMB, disk) as mb (mb)}<option value={mb}>{megabytes(mb)}</option>{/each}
				</select>
				<p class="text-sm text-muted">{t('settings.diskHint')}</p>
			</div>
		{/if}
	</form>

	<HostAccess app={g.id} />

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
