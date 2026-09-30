<script lang="ts">
	import { page } from '$app/state';
	import GameConsole from '$lib/ui/GameConsole.svelte';
</script>

<GameConsole id={page.params.app ?? ''} settings="/g/{page.params.app}/settings" base="/g/{page.params.app}" />
