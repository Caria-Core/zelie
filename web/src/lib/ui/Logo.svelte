<script lang="ts">
	// label is for when the mark stands alone; next to the name it stays hidden.
	let { size = 24, label = '' }: { size?: number; label?: string } = $props();
</script>

<svg
	xmlns="http://www.w3.org/2000/svg"
	viewBox="0 0 64 64"
	width={size}
	height={size}
	class="shrink-0"
	role={label ? 'img' : undefined}
	aria-label={label || undefined}
	aria-hidden={label ? undefined : 'true'}
>
	<circle cx="32" cy="32" r="32" class="fill-accent" />
	<path
		d="M19 20H45L19 44H45"
		fill="none"
		stroke-width="6"
		stroke-linecap="round"
		stroke-linejoin="round"
		class="stroke-accent-fg"
	/>
	<circle cx="47" cy="17" r="4" class="fill-ok" />
</svg>
