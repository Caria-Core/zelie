<script lang="ts">
	import type { HTMLButtonAttributes } from 'svelte/elements';

	let {
		kind = 'primary',
		busy = false,
		children,
		...rest
	}: HTMLButtonAttributes & { kind?: 'primary' | 'secondary' | 'quiet'; busy?: boolean } = $props();

	const styles = {
		primary: 'bg-accent text-accent-fg hover:opacity-90',
		secondary: 'bg-selected text-fg hover:bg-line',
		quiet: 'text-muted hover:text-fg'
	};
</script>

<button
	{...rest}
	disabled={busy || rest.disabled}
	class="inline-flex h-10 items-center justify-center gap-2 rounded-full px-5 text-[15px] font-medium transition disabled:opacity-50 {styles[kind]} {rest.class ?? ''}"
>
	{@render children?.()}
</button>
