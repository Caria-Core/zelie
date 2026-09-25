<script lang="ts">
	import type { Snippet } from 'svelte';
	import { t } from '$lib/i18n';
	import Button from './Button.svelte';

	let { codes, children }: { codes: string[]; children?: Snippet } = $props();
	let copied = $state(false);

	async function copy() {
		await navigator.clipboard.writeText(codes.join('\n'));
		copied = true;
	}
</script>

<ul class="grid grid-cols-2 gap-x-6 gap-y-1.5 rounded-2xl bg-panel p-5 font-mono text-[15px]">
	{#each codes as c (c)}<li>{c}</li>{/each}
</ul>
<div class="flex gap-3">
	{@render children?.()}
	<Button kind="secondary" onclick={copy}>{copied ? t('enroll.recovery.copied') : t('enroll.recovery.copy')}</Button>
</div>
