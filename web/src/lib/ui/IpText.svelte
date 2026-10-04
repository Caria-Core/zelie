<script lang="ts">
	import { maskIP } from '$lib/players';
	import { view } from '$lib/players.svelte';

	let { ip }: { ip: string } = $props();
</script>

<span class="font-mono text-sm">{ip ? (view.reveal ? ip : maskIP(ip)) : '—'}</span>
