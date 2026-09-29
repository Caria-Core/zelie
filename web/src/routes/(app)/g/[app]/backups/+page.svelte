<script lang="ts">
	import { game, gameState } from '$lib/games.svelte';
	import Backups from '$lib/ui/Backups.svelte';

	const phase = $derived(game.info ? gameState(game.info) : '');
</script>

{#if game.info}
	<Backups game app={{ id: game.info.id, state: phase === 'running' || phase === 'starting' ? 'running' : 'stopped' }} />
{/if}
