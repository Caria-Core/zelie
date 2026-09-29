<script module lang="ts">
	export type NameRequest = { title: string; label: string; value: string; action: string; hint?: string };
</script>

<script lang="ts">
	import { t } from '$lib/i18n';
	import Button from './Button.svelte';

	// Asks for one line of text, such as a new name. The parent opens it by
	// giving it a request and hears the answer once: the text, or null.

	let { request, onanswer }: { request: NameRequest | null; onanswer: (name: string | null) => void } = $props();

	let dialog = $state<HTMLDialogElement>();
	let value = $state('');
	const id = $props.id();

	$effect(() => {
		if (request && !dialog?.open) {
			value = request.value;
			dialog?.showModal();
			// Select the name without its extension, as file managers do.
			queueMicrotask(() => {
				const input = dialog?.querySelector('input');
				if (!input) return;
				input.focus();
				const dot = request.value.lastIndexOf('.');
				input.setSelectionRange(0, dot > 0 ? dot : request.value.length);
			});
		}
		if (!request && dialog?.open) dialog.close();
	});

	function done(name: string | null) {
		dialog?.close();
		onanswer(name);
	}
</script>

<dialog
	bind:this={dialog}
	oncancel={(e) => (e.preventDefault(), done(null))}
	class="m-auto w-[min(26rem,calc(100vw-2rem))] rounded-2xl border border-line bg-bg p-6 text-fg backdrop:bg-black/40"
>
	{#if request}
		<form
			class="flex flex-col gap-5"
			onsubmit={(e) => {
				e.preventDefault();
				if (value.trim()) done(value.trim());
			}}
		>
			<h2 class="text-[17px] font-medium">{request.title}</h2>
			<div class="flex flex-col gap-1.5">
				<label for={id} class="text-sm font-medium">{request.label}</label>
				<input
					{id}
					bind:value
					autocomplete="off"
					autocapitalize="off"
					spellcheck="false"
					class="h-10 w-full min-w-0 rounded-xl border border-line bg-bg px-3.5 font-mono text-[14px] outline-none transition focus:border-muted"
				/>
				{#if request.hint}<p class="text-sm text-muted">{request.hint}</p>{/if}
			</div>
			<div class="flex flex-wrap justify-end gap-2">
				<Button type="button" kind="secondary" onclick={() => done(null)}>{t('common.cancel')}</Button>
				<Button type="submit" disabled={!value.trim()}>{request.action}</Button>
			</div>
		</form>
	{/if}
</dialog>
