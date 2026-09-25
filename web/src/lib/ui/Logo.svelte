<script lang="ts">
	let { size = 'sm' }: { size?: 'sm' | 'lg' } = $props();
</script>

<span
	class="inline-flex items-center rounded-full bg-accent text-accent-fg {size === 'lg'
		? 'h-12 w-24 px-4 text-2xl'
		: 'h-6 w-11 px-2 text-sm'} font-semibold"
	aria-hidden="true">Z</span
>
