<script lang="ts">
	import { page } from '$app/state';
	import GameConsole from '$lib/ui/GameConsole.svelte';
</script>

<GameConsole id={page.params.app ?? ''} settings="/a/{page.params.app}/settings" />
