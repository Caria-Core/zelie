<script lang="ts">
	import type { HTMLInputAttributes } from 'svelte/elements';

	let {
		label,
		hint = '',
		value = $bindable(''),
		...rest
	}: HTMLInputAttributes & { label: string; hint?: string; value?: string } = $props();
	const id = $props.id();
</script>

<div class="flex min-w-0 flex-col gap-1.5">
	<label for={id} class="text-sm font-medium">{label}</label>
	<input
		{id}
		{...rest}
		bind:value
		class="h-10 w-full min-w-0 rounded-xl border border-line bg-bg px-3.5 text-[15px] outline-none transition focus:border-muted"
	/>
	{#if hint}<p class="text-sm text-muted">{hint}</p>{/if}
</div>
