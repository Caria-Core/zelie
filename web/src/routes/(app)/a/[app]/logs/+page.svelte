<script lang="ts">
	import { current } from '$lib/current.svelte';
	import LogStream from '$lib/ui/LogStream.svelte';

	const app = $derived(current.app!);
	// A new live version is a new container, so the stream starts over.
	const live = $derived(app.deployments.find((d) => d.state === 'live')?.id ?? 0);
</script>

{#key live}
	<LogStream url="/api/apps/{app.id}/logs" />
{/key}
