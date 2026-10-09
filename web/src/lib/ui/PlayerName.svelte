<script lang="ts">
	import { User } from '@lucide/svelte';
	import type { SteamInfo } from '$lib/players';

	let { name, steam = null, size = 'md' }: { name: string; steam?: SteamInfo | null; size?: 'md' | 'lg' } = $props();
	const box = $derived(size === 'lg' ? 'size-12' : 'size-8');
	const avatar = $derived(steam?.avatar ?? '');
	// A picture the browser could not load gives way to the icon.
	let broken = $state('');
</script>

<span class="flex min-w-0 items-center gap-2.5">
	{#if avatar && avatar !== broken}
		<img
			src={avatar}
			alt=""
			class="{box} shrink-0 rounded-full bg-selected object-cover"
			loading="lazy"
			referrerpolicy="no-referrer"
			onerror={() => (broken = avatar)}
		/>
	{:else}
		<span class="{box} flex shrink-0 items-center justify-center rounded-full bg-selected text-muted" aria-hidden="true"><User size={size === 'lg' ? 22 : 16} strokeWidth={1.75} /></span>
	{/if}
	<span class="truncate {size === 'lg' ? 'text-[20px] tracking-tight' : 'text-[15px] font-medium'}">{name || '—'}</span>
</span>
