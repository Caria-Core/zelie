<script lang="ts">
	import { current } from '$lib/current.svelte';
	import Backups from '$lib/ui/Backups.svelte';
</script>

{#if current.app}
	<Backups app={current.app} />
{/if}
