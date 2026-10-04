<script lang="ts">
	import { Eye, EyeOff } from '@lucide/svelte';
	import { t } from '$lib/i18n';
	import { view } from '$lib/players.svelte';
</script>

<button
	type="button"
	class="inline-flex items-center gap-1.5 rounded-lg px-2 py-1.5 text-sm text-muted transition hover:bg-hover hover:text-fg"
	aria-pressed={view.reveal}
	title={t('players.ipHint')}
	onclick={() => (view.reveal = !view.reveal)}
>
	{#if view.reveal}<EyeOff size={16} strokeWidth={1.75} />{t('players.hideIps')}{:else}<Eye size={16} strokeWidth={1.75} />{t('players.showIps')}{/if}
</button>
