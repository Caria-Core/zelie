<script lang="ts">
	import { goto } from '$app/navigation';
	import { messageOf } from '$lib/errors';
	import { cloneGame } from '$lib/games.svelte';
	import { t } from '$lib/i18n';
	import Button from './Button.svelte';
	import ErrorText from './ErrorText.svelte';

	// Makes a new server from this one. It opens when open turns true, and
	// tells the parent when it closes; on success it goes to the new server,
	// whose console shows the files being copied.
	let { id, files = false, running = false, open = $bindable(false) }: { id: string; files?: boolean; running?: boolean; open: boolean } = $props();

	let dialog = $state<HTMLDialogElement>();
	let name = $state('');
	let error = $state('');
	let busy = $state(false);
	const field = $props.id();

	$effect(() => {
		if (open && !dialog?.open) {
			name = `${id}-copy`;
			error = '';
			dialog?.showModal();
			queueMicrotask(() => {
				const input = dialog?.querySelector('input');
				input?.focus();
				input?.select();
			});
		}
		if (!open && dialog?.open) dialog.close();
	});

	async function submit(e: SubmitEvent) {
		e.preventDefault();
		busy = true;
		error = '';
		try {
			const made = await cloneGame(id, name.trim());
			open = false;
			await goto(files ? `/a/${made.id}/console` : `/g/${made.id}`);
		} catch (err) {
			error = messageOf(err);
		} finally {
			busy = false;
		}
	}
</script>

<dialog
	bind:this={dialog}
	oncancel={(e) => (e.preventDefault(), (open = false))}
	onclose={() => (open = false)}
	class="m-auto w-[min(28rem,calc(100vw-2rem))] rounded-2xl border border-line bg-bg p-6 text-fg backdrop:bg-black/40"
>
	<form class="flex flex-col gap-5" onsubmit={submit}>
		<h2 class="text-[17px] font-medium">{t('clone.title', { server: id })}</h2>
		<div class="flex flex-col gap-1.5">
			<label for={field} class="text-sm font-medium">{t('clone.name')}</label>
			<input
				id={field}
				bind:value={name}
				required
				maxlength={40}
				autocomplete="off"
				autocapitalize="off"
				spellcheck="false"
				class="h-10 w-full min-w-0 rounded-xl border border-line bg-bg px-3.5 font-mono text-[14px] outline-none transition focus:border-muted"
			/>
			<p class="text-sm text-muted">{t('clone.nameHint')}</p>
		</div>
		<ul class="flex flex-col gap-2 text-sm text-muted">
			<li>{t(files ? 'clone.copiesFiles' : 'clone.copies')}</li>
			<li>{t(files ? 'clone.notCopiedFiles' : 'clone.notCopied')}</li>
			{#if !files}<li>{t('clone.ports')}</li>{/if}
			{#if running}<li class="text-fg">{t('clone.running')}</li>{/if}
		</ul>
		<ErrorText message={error} />
		<div class="flex flex-wrap justify-end gap-2">
			<Button type="button" kind="secondary" onclick={() => (open = false)}>{t('common.cancel')}</Button>
			<Button type="submit" {busy} disabled={!name.trim()}>{t('clone.submit')}</Button>
		</div>
	</form>
</dialog>
