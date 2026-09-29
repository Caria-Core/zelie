<script lang="ts">
	import { game } from '$lib/games.svelte';
	import MetricsView from '$lib/ui/MetricsView.svelte';
</script>

{#if game.info}
	<MetricsView id={game.info.id} cpus={game.info.cpus} memoryMb={game.info.memory_mb} />
{/if}
