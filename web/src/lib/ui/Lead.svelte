<script lang="ts">
	// The house style for headings: a short statement in full colour,
	// followed by the explanation in grey.
	let { title, text = '' }: { title: string; text?: string } = $props();
</script>

<p class="text-[22px] leading-snug tracking-tight">
	<span class="text-fg">{title}</span>
	{#if text}<span class="text-muted">{text}</span>{/if}
</p>
