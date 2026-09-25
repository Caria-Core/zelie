<script lang="ts">
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { api } from '$lib/api';
	import { containers, reload } from '$lib/containers.svelte';
	import { messageOf } from '$lib/errors';
	import { t } from '$lib/i18n';
	import Button from '$lib/ui/Button.svelte';
	import ErrorText from '$lib/ui/ErrorText.svelte';
	import StateDot from '$lib/ui/StateDot.svelte';

	// The browser keeps at most this much output, so a chatty app cannot fill
	// the tab's memory.
	const keep = 256 * 1024;

	const id = $derived(page.params.id ?? '');
	const c = $derived(containers.list.find((x) => x.id === id));
	let output = $state('');
	let notice = $state('');
	let error = $state('');
	let busy = $state(false);
	let pre = $state<HTMLPreElement>();

	$effect(() => {
		const src = new EventSource(`/api/containers/${encodeURIComponent(id)}/logs`);
		output = notice = '';
		// On a reconnect the server sends the recent output again.
		src.onopen = () => (output = '');
		src.addEventListener('output', (e) => {
			const atBottom = pre ? pre.scrollHeight - pre.scrollTop - pre.clientHeight < 24 : true;
			output = (output + JSON.parse(e.data)).slice(-keep);
			if (atBottom) queueMicrotask(() => pre && (pre.scrollTop = pre.scrollHeight));
		});
		src.addEventListener('notice', (e) => {
			notice = JSON.parse(e.data);
			src.close();
		});
		return () => src.close();
	});

	async function act(fn: () => Promise<unknown>) {
		busy = true;
		error = '';
		try {
			await fn();
			await reload();
		} catch (err) {
			error = messageOf(err);
		} finally {
			busy = false;
		}
	}

	const stop = () => act(() => api('POST', `/containers/${encodeURIComponent(id)}/stop`));

	function remove() {
		if (!confirm(t('app.removeConfirm', { id }))) return;
		act(async () => {
			await api('DELETE', `/containers/${encodeURIComponent(id)}`);
			await goto('/');
		});
	}
</script>

<div class="flex h-full flex-col gap-6">
	<header class="flex flex-wrap items-start justify-between gap-4">
		<div class="flex flex-col gap-1">
			<h1 class="text-[22px] tracking-tight">{id}</h1>
			{#if c}
				<p class="flex flex-wrap items-center gap-x-3 text-sm text-muted">
					<StateDot state={c.state} label />
					<span>·</span><span class="font-mono">{c.image}</span>
					{#if c.ip}<span>·</span><span>{t('app.address')} <span class="font-mono">{c.ip}</span></span>{/if}
				</p>
			{/if}
		</div>
		<div class="flex gap-2">
			{#if c?.state === 'running'}<Button kind="secondary" {busy} onclick={stop}>{t('app.stop')}</Button>{/if}
			<Button kind="secondary" {busy} onclick={remove} class="hover:!text-danger">{t('app.remove')}</Button>
		</div>
	</header>
	<ErrorText message={error} />
	<section class="flex min-h-0 flex-1 flex-col gap-2">
		<p class="text-sm text-muted">{t('app.output')}</p>
		<pre
			bind:this={pre}
			class="h-[60vh] overflow-auto rounded-2xl bg-panel p-4 font-mono text-[13px] leading-relaxed break-all whitespace-pre-wrap">{output ||
				notice ||
				t('app.noOutput')}</pre>
	</section>
</div>
