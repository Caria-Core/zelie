<script lang="ts">
	import Brand from '$lib/ui/Brand.svelte';

	let { children } = $props();
</script>

<div class="min-h-dvh px-6 py-8">
	<header class="flex items-center gap-2.5">
		<Brand home="" />
	</header>
	<main class="mx-auto mt-[12vh] flex max-w-sm flex-col gap-8">
		{@render children()}
	</main>
</div>
