<script lang="ts">
	import { page } from '$app/state';
	import GameFiles from '$lib/ui/GameFiles.svelte';
</script>

<GameFiles base="/g/{page.params.app}" />
