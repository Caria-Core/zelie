<script lang="ts">
	import { Box, Database, FolderCode, Gamepad2, GitBranch } from '@lucide/svelte';

	// A tile for an app: where its code comes from, at a glance. Databases
	// have their own.
	let {
		source,
		database = false,
		game = false,
		size = 'md'
	}: { source?: 'github' | 'image' | 'files'; database?: boolean; game?: boolean; size?: 'sm' | 'md' | 'lg' } = $props();
	const box = { sm: 'size-6 rounded-md', md: 'size-10 rounded-xl', lg: 'size-12 rounded-2xl' };
	const icon = { sm: 14, md: 20, lg: 24 };
</script>

<span class="inline-flex shrink-0 items-center justify-center border border-line bg-panel text-fg {box[size]}" aria-hidden="true">
	{#if database}<Database size={icon[size]} strokeWidth={1.5} />{:else if game}<Gamepad2 size={icon[size]} strokeWidth={1.5} />{:else if source === 'github'}<GitBranch
			size={icon[size]}
			strokeWidth={1.5}
		/>{:else if source === 'files'}<FolderCode size={icon[size]} strokeWidth={1.5} />{:else}<Box size={icon[size]} strokeWidth={1.5} />{/if}
</span>
