<script lang="ts">
	import { ArrowDown, LoaderCircle, Send, Stethoscope } from '@lucide/svelte';
	import { untrack } from 'svelte';
	import { api, ApiError } from '$lib/api';
	import { parseAnsi, type Span } from '$lib/ansi';
	import { acceptEula, applyFix, eulaLink, game, gameState, loadDiagnosis, loadGame, power, type Diagnosis, type DiagnosisFix } from '$lib/games.svelte';
	import { say, t, type Key } from '$lib/i18n';
	import { reload } from '$lib/apps.svelte';
	import { ask } from '$lib/ask.svelte';
	import { messageOf } from '$lib/errors';
	import Button from './Button.svelte';

	// settings is where a failed install can be run again. base is the
	// server's own pages, such as /g/survival.
	let { id, settings, base }: { id: string; settings: string; base: string } = $props();

	type Line = { n: number; spans: Span[]; kind: 'line' | 'install' | 'notice' };

	// The DOM keeps this many lines; a busy game would otherwise slow the
	// tab down.
	const keep = 2000;
	let lines = $state<Line[]>([]);
	let text = $state('');
	let connected = $state(false);
	let everConnected = $state(false);
	let stopped = $state('');
	let installed = $state(false);
	// The server this one was copied from, remembered for the message that
	// follows the copy.
	let copy = $state({ id: '', from: '' });
	const copiedFrom = $derived(copy.id === id ? copy.from : '');
	let scroller = $state<HTMLElement>();
	let atBottom = $state(true);

	let sock: WebSocket | null = null;
	let history: string[] = [];
	// Where ↑ and ↓ are in the history; equal to its length when typing anew.
	let cursor = 0;
	let draft = '';
	let counter = 0;
	let pending: Line[] = [];
	let frame = 0;
	let clearNext = false;

	const info = $derived(game.info?.id === id ? game.info : null);
	const phase = $derived(info ? gameState(info) : '');
	const canType = $derived(connected && (phase === 'running' || phase === 'starting'));
	const installing = $derived(phase === 'installing');
	const eulaNeeded = $derived(!!info?.eula_needed && info.install.state === 'installed' && phase !== 'running' && phase !== 'starting');
	$effect(() => {
		if (info?.install.clone_of) copy = { id, from: info.install.clone_of };
	});
	const failed = $derived(!installing && info?.install.state === 'failed');

	let eulaOpen = false;
	let eulaError = $state('');

	// Offered when the server refused to start for want of the EULA, or was
	// found to need it. Accepting records it and starts the server.
	async function askEula() {
		if (eulaOpen) return;
		eulaOpen = true;
		eulaError = '';
		try {
			const ok = await ask({
				title: t('console.eulaTitle'),
				text: t('console.eulaText'),
				link: { href: eulaLink, label: t('console.eulaLink') },
				action: t('console.eulaAccept')
			});
			if (!ok) return;
			await acceptEula(id);
			await power(id, 'start');
		} catch (err) {
			eulaError = messageOf(err);
		} finally {
			eulaOpen = false;
		}
	}

	// The crash doctor: asked once each time the server has crashed or its
	// install failed.
	let diag = $state<Diagnosis | null>(null);
	let applied = $state(false);
	let fixing = $state(false);
	let fixError = $state('');
	const needsDoctor = $derived(phase === 'crashed' || failed);

	$effect(() => {
		const server = id;
		if (!needsDoctor) {
			diag = null;
			return;
		}
		let stale = false;
		loadDiagnosis(server)
			.then((d) => !stale && (diag = d.cause ? d : null))
			.catch(() => {});
		return () => (stale = true);
	});

	$effect(() => {
		if (phase === 'starting' || phase === 'running' || phase === 'installing') applied = false;
	});

	const imageLabel = (ref: string) => info?.images.find((i) => i.ref === ref)?.label ?? ref;
	const fixVars = (fix: DiagnosisFix) => ({ size: String(fix.params?.size ?? ''), image: imageLabel(String(fix.params?.image ?? '')) });
	const fixLabel = (fix: DiagnosisFix) => t(`diagnosis.fix.${fix.kind}` as Key, fixVars(fix));
	const fixLinks: Partial<Record<DiagnosisFix['kind'], string>> = { open_network: 'network', open_backups: 'backups' };

	// Fixes that change the server ask first, like the settings pages do.
	async function runFix(fix: DiagnosisFix) {
		if (fixing) return;
		fixing = true;
		fixError = '';
		try {
			const ok = await ask({
				title: fixLabel(fix),
				text: t(`diagnosis.confirm.${fix.kind}` as Key, fixVars(fix)),
				link: fix.kind === 'accept_eula' ? { href: eulaLink, label: t('console.eulaLink') } : undefined,
				action: fixLabel(fix)
			});
			if (!ok) return;
			await applyFix(id, fix);
			diag = null;
			applied = fix.kind !== 'reinstall';
		} catch (err) {
			fixError = messageOf(err);
		} finally {
			fixing = false;
		}
	}

	async function startAgain() {
		if (fixing) return;
		fixing = true;
		fixError = '';
		try {
			await power(id, 'start');
		} catch (err) {
			fixError = messageOf(err);
		} finally {
			fixing = false;
		}
	}

	function flush() {
		frame = 0;
		const near = atBottom;
		if (clearNext) {
			lines = [];
			clearNext = false;
		}
		lines = [...lines, ...pending].slice(-keep);
		pending = [];
		if (near) queueMicrotask(toBottom);
	}

	function add(data: string, kind: Line['kind']) {
		pending.push({ n: counter++, spans: kind === 'notice' ? [{ text: data, style: '' }] : parseAnsi(data), kind });
		if (!frame) frame = requestAnimationFrame(flush);
	}

	function toBottom() {
		if (scroller) scroller.scrollTop = scroller.scrollHeight;
		atBottom = true;
	}

	function scrolled() {
		if (scroller) atBottom = scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight < 24;
	}

	function handle(raw: string) {
		let m: { type: string; [k: string]: unknown };
		try {
			m = JSON.parse(raw);
		} catch {
			return;
		}
		switch (m.type) {
			case 'history':
				// The first message of a connection; the lines that follow are
				// the recent ones again, so what is shown starts over.
				history = (m.commands as string[]) ?? [];
				cursor = history.length;
				clearNext = true;
				pending = [];
				if (!frame) frame = requestAnimationFrame(flush);
				break;
			case 'line':
				add(String(m.data ?? ''), 'line');
				break;
			case 'install':
				add(String(m.data ?? ''), 'install');
				break;
			case 'state': {
				const before = untrack(() => (game.info ? gameState(game.info) : ''));
				game.live = String(m.state);
				// An install ends here: learn how it went.
				if (before === 'installing' && m.state !== 'installing') {
					loadGame(id).then(() => (installed = game.info?.install.state === 'installed'));
					reload().catch(() => {});
				}
				break;
			}
			case 'eula':
				// The game may have been started by the supervisor meanwhile.
				loadGame(id).then(() => {
					if (game.info?.eula_needed) askEula();
				});
				break;
			case 'error':
				add(say({ code: String(m.code), params: m.params as Record<string, unknown>, text: String(m.message ?? '') }), 'notice');
				break;
		}
	}

	$effect(() => {
		const server = id;
		let dead = false;
		let timer: ReturnType<typeof setTimeout>;
		let tries = 0;
		lines = [];
		pending = [];
		history = [];
		cursor = 0;
		connected = everConnected = false;
		stopped = '';
		installed = false;

		async function connect() {
			if (dead) return;
			try {
				const { token } = await api<{ token: string }>('POST', `/games/${encodeURIComponent(server)}/console/token`);
				if (dead) return;
				const scheme = location.protocol === 'https:' ? 'wss' : 'ws';
				const ws = new WebSocket(`${scheme}://${location.host}/api/games/${encodeURIComponent(server)}/console?token=${encodeURIComponent(token)}`);
				sock = ws;
				ws.onopen = () => {
					connected = everConnected = true;
					tries = 0;
				};
				ws.onmessage = (e) => typeof e.data === 'string' && handle(e.data);
				ws.onclose = () => {
					if (sock === ws) sock = null;
					connected = false;
					game.live = '';
					retry();
				};
			} catch (err) {
				// Someone who lost access, or a server that is gone, will not get
				// it back by asking again.
				if (err instanceof ApiError && (err.status === 403 || err.status === 404)) {
					stopped = say(err.msg);
					return;
				}
				retry();
			}
		}
		function retry() {
			if (dead) return;
			timer = setTimeout(connect, Math.min(15000, 1000 * 2 ** tries++));
		}
		connect();
		return () => {
			dead = true;
			clearTimeout(timer);
			cancelAnimationFrame(frame);
			frame = 0;
			sock?.close();
			sock = null;
			game.live = '';
		};
	});

	function send(e: SubmitEvent) {
		e.preventDefault();
		const cmd = text.trim();
		if (!cmd || !canType || sock?.readyState !== WebSocket.OPEN) return;
		sock.send(JSON.stringify({ type: 'command', data: cmd }));
		if (history[history.length - 1] !== cmd) history.push(cmd);
		if (history.length > 100) history.shift();
		cursor = history.length;
		draft = text = '';
	}

	function keys(e: KeyboardEvent) {
		if (e.key !== 'ArrowUp' && e.key !== 'ArrowDown') return;
		if (!history.length) return;
		e.preventDefault();
		if (e.key === 'ArrowUp') {
			if (cursor === history.length) draft = text;
			cursor = Math.max(0, cursor - 1);
		} else cursor = Math.min(history.length, cursor + 1);
		text = cursor === history.length ? draft : history[cursor];
	}
</script>

<div class="flex flex-col gap-4">
	{#if eulaNeeded}
		<div class="flex flex-wrap items-center justify-between gap-3 rounded-xl border border-line px-4 py-3" role="status">
			<p class="text-sm font-medium">{t('console.eulaBanner')}</p>
			<Button kind="secondary" onclick={askEula}>{t('console.eulaReview')}</Button>
		</div>
		{#if eulaError}<p role="alert" class="text-sm text-danger">{eulaError}</p>{/if}
	{/if}
	{#if installing}
		<div class="flex items-start gap-3 rounded-xl border border-line px-4 py-3" role="status">
			<LoaderCircle size={18} class="mt-0.5 shrink-0 animate-spin text-muted" />
			<div class="min-w-0">
				{#if info?.install.clone_of}
					<p class="text-sm font-medium">{t('console.copying')}</p>
					<p class="text-sm text-muted">{t('console.copyingText', { server: info.install.clone_of })}</p>
				{:else}
					<p class="text-sm font-medium">{t('console.installing')}</p>
					<p class="text-sm text-muted">{t('console.installingText')}</p>
				{/if}
			</div>
		</div>
	{:else if failed}
		<div class="rounded-xl border border-danger/30 px-4 py-3" role="status">
			<p class="text-sm font-medium text-danger">{t('console.installFailed')}</p>
			<p class="text-sm text-muted">
				{t('console.installFailedText')}
				<a href={settings} class="text-fg underline decoration-line underline-offset-2 hover:decoration-fg">{t('game.tab.settings')}</a>
			</p>
		</div>
	{:else if installed && phase !== 'running'}
		<div class="rounded-xl border border-line px-4 py-3" role="status">
			{#if copiedFrom}
				<p class="text-sm font-medium text-ok">{t('console.copied')}</p>
				<p class="text-sm text-muted">{t(info?.ports.length ? 'console.copiedText' : 'console.copiedFilesText', { server: copiedFrom })}</p>
			{:else}
				<p class="text-sm font-medium text-ok">{t('console.installed')}</p>
				<p class="text-sm text-muted">{t('console.installedText')}</p>
			{/if}
		</div>
	{/if}

	{#if diag?.cause && !(eulaNeeded && diag.fix?.kind === 'accept_eula')}
		{@const fix = diag.fix}
		<div class="flex flex-wrap items-start gap-3 rounded-xl border border-line px-4 py-3" role="status">
			<Stethoscope size={18} class="mt-0.5 shrink-0 text-muted" />
			<div class="min-w-0 flex-1 basis-56">
				<p class="text-sm font-medium">{t('diagnosis.title')}</p>
				<p class="text-sm text-muted">{say(diag.cause)}</p>
			</div>
			{#if fix && fixLinks[fix.kind]}
				<a
					href="{base}/{fixLinks[fix.kind]}"
					class="inline-flex h-10 w-full items-center justify-center rounded-full bg-selected px-5 text-[15px] font-medium text-fg transition hover:bg-line sm:w-auto"
					>{fixLabel(fix)}</a
				>
			{:else if fix}
				<Button kind="secondary" class="w-full sm:w-auto" busy={fixing} onclick={() => runFix(fix)}>{fixLabel(fix)}</Button>
			{/if}
		</div>
	{:else if applied && phase !== 'running' && phase !== 'starting' && phase !== 'installing'}
		<div class="flex flex-wrap items-center gap-3 rounded-xl border border-line px-4 py-3" role="status">
			<p class="min-w-0 flex-1 basis-56 text-sm font-medium text-ok">{t('diagnosis.done')}</p>
			<Button class="w-full sm:w-auto" busy={fixing} onclick={startAgain}>{t('game.start')}</Button>
		</div>
	{/if}
	{#if fixError}<p role="alert" class="text-sm text-danger">{fixError}</p>{/if}

	<div class="relative">
		<div
			bind:this={scroller}
			onscroll={scrolled}
			role="log"
			aria-label={t('game.tab.console')}
			class="h-[60vh] min-h-72 overflow-auto rounded-2xl border border-line bg-[#141414] p-4 font-mono text-[13px] leading-relaxed text-[#d4d4d4]"
		>
			{#each lines as l (l.n)}
				<div class="min-h-[1.4em] break-all whitespace-pre-wrap {l.kind === 'notice' ? 'text-[#8a8a8a] italic' : ''}">{#each l.spans as s, i (i)}<span style={s.style}>{s.text}</span>{/each}</div>
			{:else}
				<p class="text-[#8a8a8a]">{connected ? t('console.empty') : stopped || t('console.connecting')}</p>
			{/each}
		</div>
		{#if !atBottom}
			<button
				class="absolute right-4 bottom-4 inline-flex items-center gap-1.5 rounded-full bg-accent px-3.5 py-1.5 text-sm font-medium text-accent-fg shadow-lg transition hover:opacity-90"
				onclick={toBottom}><ArrowDown size={14} />{t('console.jump')}</button
			>
		{/if}
	</div>

	{#if !connected && (everConnected || lines.length > 0) && !stopped}
		<p class="text-sm text-muted" role="status">{t('console.reconnecting')}</p>
	{:else if stopped}
		<p role="alert" class="text-sm text-danger">{stopped}</p>
	{/if}

	<form class="flex items-center gap-2" onsubmit={send}>
		<input
			class="h-10 min-w-0 flex-1 rounded-xl border border-line bg-bg px-3.5 font-mono text-[15px] outline-none transition focus:border-muted disabled:opacity-60"
			aria-label={t('console.command')}
			placeholder={canType ? t('console.commandPlaceholder') : t('console.offline')}
			autocomplete="off"
			autocapitalize="off"
			spellcheck="false"
			maxlength={1024}
			disabled={!canType}
			bind:value={text}
			onkeydown={keys}
		/>
		<Button type="submit" class="!px-4" disabled={!canType || !text.trim()} aria-label={t('console.send')}><Send size={16} /></Button>
	</form>
</div>
