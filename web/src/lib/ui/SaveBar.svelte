<script lang="ts">
	import { RotateCcw, Undo2 } from '@lucide/svelte';
	import { beforeNavigate, goto } from '$app/navigation';
	import { ask } from '$lib/ask.svelte';
	import { t } from '$lib/i18n';
	import Button from '$lib/ui/Button.svelte';

	// Sits at the top of a form and stays in view while its page scrolls.
	// Put it inside the <form>, as the first thing, so it sticks for as long
	// as the form is on screen.
	let {
		dirty,
		busy = false,
		canUndo = false,
		valid = true,
		onsave,
		onundo,
		onreset
	}: { dirty: boolean; busy?: boolean; canUndo?: boolean; valid?: boolean; onsave: () => void; onundo: () => void; onreset: () => void } = $props();

	const canSave = $derived(dirty && valid && !busy);

	// Text fields keep their own undo; the shortcut is for everything else.
	function typing(el: EventTarget | null): boolean {
		if (!(el instanceof HTMLElement)) return false;
		if (el.isContentEditable || el instanceof HTMLTextAreaElement) return true;
		return el instanceof HTMLInputElement && !['checkbox', 'radio', 'range', 'button', 'submit'].includes(el.type);
	}

	function keys(e: KeyboardEvent) {
		if (!(e.metaKey || e.ctrlKey) || e.altKey) return;
		const key = e.key.toLowerCase();
		if (key === 's') {
			e.preventDefault();
			if (canSave) onsave();
		} else if (key === 'z' && !e.shiftKey && !typing(e.target) && canUndo) {
			e.preventDefault();
			onundo();
		}
	}

	let leaving = false;
	beforeNavigate((nav) => {
		if (!dirty || leaving || nav.willUnload || !nav.to) return;
		nav.cancel();
		const to = nav.to.url;
		ask({ title: t('savebar.leaveTitle'), text: t('savebar.leaveText'), action: t('savebar.leave'), danger: true }).then((ok) => {
			if (!ok) return;
			leaving = true;
			goto(to).finally(() => (leaving = false));
		});
	});
</script>

<svelte:window
	onkeydown={keys}
	onbeforeunload={(e) => {
		if (dirty) e.preventDefault();
	}}
/>

<div
	class="sticky top-2 z-[5] flex items-center justify-between gap-2 rounded-2xl border border-line bg-bg/90 py-2 pr-2 pl-4 shadow-sm backdrop-blur"
	role="group"
	aria-label={t('savebar.label')}
>
	<p class="flex min-w-0 items-center gap-2 text-sm {dirty ? 'text-fg' : 'text-muted'}" role="status">
		<span class="size-2 shrink-0 rounded-full {dirty ? 'bg-warn' : 'bg-ok'}"></span>
		<span class="truncate">{dirty ? t('savebar.unsaved') : t('savebar.saved')}</span>
	</p>
	<div class="flex shrink-0 items-center gap-1">
		<Button type="button" kind="quiet" class="!h-9 !px-3 !text-sm" disabled={!canUndo || busy} title={t('savebar.undoHint')} aria-label={t('savebar.undo')} onclick={onundo}>
			<Undo2 size={15} strokeWidth={1.75} /><span class="hidden sm:inline">{t('savebar.undo')}</span>
		</Button>
		<Button type="button" kind="quiet" class="!h-9 !px-3 !text-sm" disabled={!dirty || busy} title={t('savebar.resetHint')} aria-label={t('savebar.reset')} onclick={onreset}>
			<RotateCcw size={15} strokeWidth={1.75} /><span class="hidden sm:inline">{t('savebar.reset')}</span>
		</Button>
		<Button type="button" class="!h-9 !px-4 !text-sm" {busy} disabled={!canSave} onclick={onsave}>{t('savebar.save')}</Button>
	</div>
</div>
