<script lang="ts">
	let { message }: { message: string } = $props();
</script>

{#if message}
	<p role="alert" class="text-sm text-danger">{message}</p>
{/if}
