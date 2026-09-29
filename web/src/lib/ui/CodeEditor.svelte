<script lang="ts">
	import { onMount } from 'svelte';
	import type { Handle } from '$lib/editor/cm';
	import type { Problem } from '$lib/editor/json';
	import { t } from '$lib/i18n';

	let {
		value = $bindable(''),
		path,
		label,
		onsave,
		onproblem
	}: {
		value: string;
		path: string;
		label: string;
		onsave: () => void;
		onproblem?: (problem: Problem | null) => void;
	} = $props();

	let host = $state<HTMLDivElement>();
	let handle: Handle | null = null;
	let failed = $state(false);
	// The text as the editor last said it, so an edit that came from the
	// editor is not sent back to it.
	let last = '';

	export function reveal(line: number, column: number) {
		handle?.reveal(line, column);
	}

	onMount(() => {
		let gone = false;
		const root = host!.attachShadow({ mode: 'open' });
		const parent = document.createElement('div');
		parent.style.height = '100%';
		root.append(parent);
		import('$lib/editor/cm')
			.then(({ createEditor }) =>
				createEditor({
					root,
					parent,
					path,
					text: value,
					label,
					onchange: (text) => {
						last = text;
						value = text;
					},
					onsave: () => onsave(),
					onproblem: (p) => onproblem?.(p)
				})
			)
			.then((h) => {
				if (gone) return h.destroy();
				last = value;
				handle = h;
			})
			.catch(() => (failed = true));
		return () => {
			gone = true;
			handle?.destroy();
			handle = null;
		};
	});

	$effect(() => {
		const text = value;
		if (handle && text !== last) {
			last = text;
			handle.set(text);
		}
	});
</script>

<div class="code-editor h-[min(65vh,42rem)] min-h-64 overflow-hidden rounded-2xl border border-line bg-bg transition focus-within:border-muted">
	{#if failed}
		<p class="p-4 text-sm text-danger">{t('files.editorFailed')}</p>
	{:else}
		<div bind:this={host} class="h-full"></div>
	{/if}
</div>

<style>
	/* The editor draws itself with these, so a theme only changes them here. */
	.code-editor {
		--cm-selection: #d9e4f5;
		--cm-match: #eef0f3;
		--cm-error-bg: #f8dcdc;
		--cm-keyword: #7a4fb5;
		--cm-string: #2f7d4f;
		--cm-number: #b0601c;
		--cm-comment: #8a8a8a;
		--cm-property: #2563a8;
		--cm-type: #9a5a3c;
		--cm-tag: #b23a48;
		--cm-punct: #6b6b6b;
	}

	@media (prefers-color-scheme: dark) {
		:global(:root:not([data-theme='light'])) .code-editor {
			--cm-selection: #2c3d57;
			--cm-match: #2a2a2a;
			--cm-error-bg: #4a2626;
			--cm-keyword: #b794f4;
			--cm-string: #7fc79b;
			--cm-number: #e0a86a;
			--cm-comment: #7a7a7a;
			--cm-property: #7fb2e5;
			--cm-type: #e0a08a;
			--cm-tag: #e88a95;
			--cm-punct: #a0a0a0;
		}
	}

	:global(:root[data-theme='dark']) .code-editor {
		--cm-selection: #2c3d57;
		--cm-match: #2a2a2a;
		--cm-error-bg: #4a2626;
		--cm-keyword: #b794f4;
		--cm-string: #7fc79b;
		--cm-number: #e0a86a;
		--cm-comment: #7a7a7a;
		--cm-property: #7fb2e5;
		--cm-type: #e0a08a;
		--cm-tag: #e88a95;
		--cm-punct: #a0a0a0;
	}
</style>
