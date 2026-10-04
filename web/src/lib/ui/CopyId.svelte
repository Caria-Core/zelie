<script lang="ts">
	import { Check, Copy } from '@lucide/svelte';
	import { t } from '$lib/i18n';

	let { text }: { text: string } = $props();
	let copied = $state(false);

	async function copy() {
		try {
			await navigator.clipboard.writeText(text);
			copied = true;
			setTimeout(() => (copied = false), 1500);
		} catch {
			// The id is on screen to select by hand.
		}
	}
</script>

<span class="inline-flex min-w-0 items-center gap-1">
	<span class="truncate font-mono text-sm select-all">{text}</span>
	<button
		type="button"
		class="rounded-md p-1 text-muted transition hover:bg-hover hover:text-fg"
		aria-label={t('players.copyId')}
		title={copied ? t('game.copied') : t('players.copyId')}
		onclick={copy}
	>
		{#if copied}<Check size={14} />{:else}<Copy size={14} />{/if}
	</button>
</span>
