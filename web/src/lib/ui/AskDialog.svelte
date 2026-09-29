<script lang="ts">
	import { asking } from '$lib/ask.svelte';
	import { t } from '$lib/i18n';
	import Button from './Button.svelte';

	let dialog = $state<HTMLDialogElement>();

	$effect(() => {
		if (asking.question && !dialog?.open) dialog?.showModal();
		if (!asking.question && dialog?.open) dialog.close();
	});

	function done(ok: boolean) {
		dialog?.close();
		asking.answer?.(ok);
	}
</script>

<!-- Escape closes the dialog through the cancel event, which counts as no.
     Cancel comes first, so it is what has focus when the dialog opens. -->
<dialog
	bind:this={dialog}
	oncancel={(e) => (e.preventDefault(), done(false))}
	class="m-auto w-[min(26rem,calc(100vw-2rem))] rounded-2xl border border-line bg-bg p-6 text-fg backdrop:bg-black/40"
>
	{#if asking.question}
		{@const q = asking.question}
		<div class="flex flex-col gap-6">
			<div class="flex flex-col gap-2">
				<h2 class="text-[17px] font-medium">{q.title}</h2>
				{#if q.text}<p class="text-[15px] text-muted">{q.text}</p>{/if}
				{#if q.link}
					<a href={q.link.href} target="_blank" rel="noopener noreferrer" class="w-fit text-[15px] underline decoration-line underline-offset-2 hover:decoration-fg">{q.link.label}</a>
				{/if}
			</div>
			<div class="flex flex-wrap justify-end gap-2">
				<Button kind="secondary" onclick={() => done(false)}>{t('common.cancel')}</Button>
				<Button kind={q.danger ? 'danger' : 'primary'} onclick={() => done(true)}>{q.action}</Button>
			</div>
		</div>
	{/if}
</dialog>
