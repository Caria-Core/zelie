<script lang="ts">
	import { current } from '$lib/current.svelte';
	import MetricsView from '$lib/ui/MetricsView.svelte';

	const app = $derived(current.app!);
</script>

<MetricsView id={app.id} domain={app.domain} cpus={app.cpus} memoryMb={app.memory_mb} />
