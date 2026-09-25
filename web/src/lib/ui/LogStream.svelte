<script lang="ts">
	import { t } from '$lib/i18n';

	// LogStream shows server-sent output as it arrives: "output" events
	// carry text, "notice" and "done" end the stream.
	let { url, height = 'h-[60vh]', ondone }: { url: string; height?: string; ondone?: (state: string) => void } = $props();

	// The browser keeps at most this much, so a chatty app cannot fill the
	// tab's memory.
	const keep = 256 * 1024;
	let output = $state('');
	let notice = $state('');
	let pre = $state<HTMLPreElement>();

	// The parent builds url from objects that are replaced on every refresh.
	// A derived value only changes when the string does, so the stream is
	// not reopened each time.
	const target = $derived(url);

	$effect(() => {
		const src = new EventSource(target);
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
		src.addEventListener('done', (e) => {
			src.close();
			ondone?.(JSON.parse(e.data));
		});
		return () => src.close();
	});
</script>

<pre
	bind:this={pre}
	class="{height} overflow-auto rounded-2xl bg-panel p-4 font-mono text-[13px] leading-relaxed break-all whitespace-pre-wrap">{output ||
		notice ||
		t('app.noOutput')}</pre>
