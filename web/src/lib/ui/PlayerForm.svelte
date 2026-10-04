<script lang="ts">
	import { page } from '$app/state';
	import { api } from '$lib/api';
	import { messageOf } from '$lib/errors';
	import { t, type Key } from '$lib/i18n';
	import { banDurations, noteTags } from '$lib/players';
	import { changed, view } from '$lib/players.svelte';
	import Button from './Button.svelte';
	import ErrorText from './ErrorText.svelte';

	// One dialog for the three actions that ask something first: kick,
	// ban and add a note. view.form opens it.
	const app = $derived(page.params.app ?? '');
	let dialog = $state<HTMLDialogElement>();
	let reason = $state('');
	let minutes = $state<number>(1440);
	let tag = $state('');
	let busy = $state(false);
	let error = $state('');
	const id = $props.id();

	$effect(() => {
		if (view.form && !dialog?.open) {
			reason = '';
			minutes = 1440;
			tag = '';
			error = '';
			dialog?.showModal();
		}
		if (!view.form && dialog?.open) dialog.close();
	});

	const close = () => {
		dialog?.close();
		view.form = null;
	};

	async function submit(e: SubmitEvent) {
		e.preventDefault();
		const f = view.form;
		if (!f || busy) return;
		busy = true;
		error = '';
		const path = `/games/${encodeURIComponent(app)}/players/${encodeURIComponent(f.id)}`;
		try {
			if (f.kind === 'kick') await api('POST', `${path}/kick`, { reason: reason.trim() });
			else if (f.kind === 'ban') await api('POST', `${path}/ban`, { reason: reason.trim(), minutes });
			else await api('POST', `${path}/notes`, { tag, note: reason.trim() });
			changed();
			close();
		} catch (err) {
			error = messageOf(err);
		} finally {
			busy = false;
		}
	}
</script>

<dialog
	bind:this={dialog}
	oncancel={(e) => (e.preventDefault(), close())}
	class="m-auto w-[min(26rem,calc(100vw-2rem))] rounded-2xl border border-line bg-bg p-6 text-fg backdrop:bg-black/40"
>
	{#if view.form}
		{@const f = view.form}
		<form class="flex flex-col gap-5" onsubmit={submit}>
			<div class="flex flex-col gap-1.5">
				<h2 class="text-[17px] font-medium">{t(`players.${f.kind}Title` as Key, { name: f.name })}</h2>
				<p class="text-sm text-muted">{t(`players.${f.kind}Lead` as Key)}</p>
			</div>
			{#if f.kind === 'note'}
				<div class="flex flex-col gap-1.5">
					<label for="{id}-tag" class="text-sm font-medium">{t('players.noteTag')}</label>
					<select id="{id}-tag" bind:value={tag} class="h-10 rounded-xl border border-line bg-bg px-3 text-[15px]">
						{#each noteTags as k (k)}<option value={k}>{k ? t(`players.tag.${k}` as Key) : t('players.tag.none')}</option>{/each}
					</select>
				</div>
			{/if}
			<div class="flex flex-col gap-1.5">
				<label for="{id}-text" class="text-sm font-medium">{f.kind === 'note' ? t('players.noteText') : t('players.reason')}</label>
				{#if f.kind === 'note'}
					<textarea id="{id}-text" bind:value={reason} rows="3" maxlength="500" class="w-full rounded-xl border border-line bg-bg px-3.5 py-2 text-[15px] outline-none transition focus:border-muted"></textarea>
				{:else}
					<input id="{id}-text" bind:value={reason} maxlength="200" autocomplete="off" class="h-10 w-full rounded-xl border border-line bg-bg px-3.5 text-[15px] outline-none transition focus:border-muted" />
				{/if}
			</div>
			{#if f.kind === 'ban'}
				<div class="flex flex-col gap-1.5">
					<label for="{id}-time" class="text-sm font-medium">{t('players.duration')}</label>
					<select id="{id}-time" bind:value={minutes} class="h-10 rounded-xl border border-line bg-bg px-3 text-[15px]">
						{#each banDurations as m (m)}<option value={m}>{t(`players.dur.${m}` as Key)}</option>{/each}
					</select>
					<p class="text-sm text-muted">{t('players.durationHint')}</p>
				</div>
			{/if}
			<ErrorText message={error} />
			<div class="flex flex-wrap justify-end gap-2">
				<Button type="button" kind="secondary" onclick={close}>{t('common.cancel')}</Button>
				<Button type="submit" kind={f.kind === 'ban' ? 'danger' : 'primary'} {busy} disabled={f.kind === 'note' && !reason.trim()}>{t(`players.${f.kind}Action` as Key)}</Button>
			</div>
		</form>
	{/if}
</dialog>
