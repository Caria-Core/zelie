<script lang="ts">
	import { FileUp, LoaderCircle, Upload } from '@lucide/svelte';
	import { api, ApiError, errorFrom } from '$lib/api';
	import type { Engine } from '$lib/apps.svelte';
	import { bytes } from '$lib/backups';
	import { sensitive } from '$lib/confirm.svelte';
	import { Cancelled, messageOf } from '$lib/errors';
	import { list, t } from '$lib/i18n';
	import Button from '$lib/ui/Button.svelte';
	import ErrorText from '$lib/ui/ErrorText.svelte';

	let {
		db,
		engine,
		apps,
		keepDays,
		disabled = false,
		onstarted
	}: { db: string; engine: Engine; apps: string[]; keepDays: number; disabled?: boolean; onstarted: () => void } = $props();

	type Started = { id: string; size: number; received: number; chunk: number };

	const types = $derived(engine === 'redis' ? '.rdb, .rdb.gz, .rdb.zst' : '.sql, .sql.gz, .sql.zst');
	const redis = $derived(engine === 'redis');

	let dialog = $state<HTMLDialogElement>();
	let file = $state<File | null>(null);
	let dragging = $state(false);
	let error = $state('');
	// Set once the upload runs: how much the server has, and whether a
	// dropped connection is being retried.
	let sent = $state<number | null>(null);
	let retrying = $state(false);
	let stopped = $state(false);
	let upload: Started | null = null;
	let xhr: XMLHttpRequest | null = null;
	let cancelled = false;

	function open() {
		file = null;
		error = '';
		sent = null;
		stopped = false;
		dialog?.showModal();
	}

	function pick(f: File | undefined) {
		if (!f) return;
		file = f;
		error = '';
	}

	function drop(e: DragEvent) {
		e.preventDefault();
		dragging = false;
		pick(e.dataTransfer?.files[0]);
	}

	// put sends one piece and reports progress within it. XMLHttpRequest,
	// since fetch cannot tell how much of a body went out.
	function put(u: Started, offset: number, piece: Blob): Promise<Started> {
		return new Promise((resolve, reject) => {
			const req = new XMLHttpRequest();
			xhr = req;
			req.open('PUT', `/api/apps/${db}/uploads/${u.id}?offset=${offset}`);
			req.setRequestHeader('Content-Type', 'application/octet-stream');
			req.upload.onprogress = (e) => (sent = offset + e.loaded);
			req.onload = () => {
				let data: Record<string, unknown> = {};
				try {
					data = JSON.parse(req.responseText);
				} catch {
					/* not JSON: a proxy's page, say */
				}
				if (req.status >= 200 && req.status < 300) resolve(data as Started);
				else reject(errorFrom(req.status, req.statusText, data));
			};
			req.onerror = () => reject(new TypeError('network'));
			req.onabort = () => reject(new Cancelled());
			req.send(piece);
		});
	}

	const wait = (ms: number) => new Promise((r) => setTimeout(r, ms));

	// send goes on from what the server has until the file is there. A
	// dropped connection is tried again a few times, a little later each
	// time; the server says where to go on from.
	async function send(u: Started, f: File) {
		let offset = u.received;
		let tries = 0;
		sent = offset;
		while (offset < u.size) {
			if (cancelled) throw new Cancelled();
			try {
				const got = await put(u, offset, f.slice(offset, offset + u.chunk));
				offset = got.received;
				tries = 0;
				retrying = false;
			} catch (err) {
				if (err instanceof ApiError && err.msg.code === 'upload.offset') {
					offset = Number(err.msg.params?.received ?? 0);
					continue;
				}
				if (err instanceof ApiError || err instanceof Cancelled || ++tries > 5) throw err;
				retrying = true;
				await wait(1000 * 2 ** tries);
			}
			sent = offset;
		}
	}

	async function start() {
		const f = file!;
		error = '';
		stopped = false;
		cancelled = false;
		try {
			upload = await sensitive(() => api<Started>('POST', `/apps/${db}/uploads`, { name: f.name, size: f.size }));
			await send(upload, f);
			await api('POST', `/apps/${db}/uploads/${upload.id}/finish`);
			upload = null;
			dialog?.close();
			onstarted();
		} catch (err) {
			retrying = false;
			if (err instanceof Cancelled) return;
			error = messageOf(err);
			// Before anything was sent, the file can simply be changed.
			stopped = sent !== null;
		}
	}

	async function stop() {
		cancelled = true;
		xhr?.abort();
		const u = upload;
		upload = null;
		sent = null;
		stopped = false;
		retrying = false;
		if (u) await api('DELETE', `/apps/${db}/uploads/${u.id}`).catch(() => {});
		dialog?.close();
	}

	const running = $derived(sent !== null && !stopped && !error);
	const percent = $derived(file && sent !== null ? Math.floor((sent / file.size) * 100) : 0);
	const days = $derived(t('backups.days', { n: keepDays }));
</script>

<Button kind="secondary" onclick={open} {disabled} title={disabled ? t('backups.notRunning') : ''}
	><FileUp size={16} strokeWidth={1.75} />{t('backups.file.open')}</Button
>

<dialog
	bind:this={dialog}
	class="m-auto w-[min(32rem,calc(100vw-2rem))] rounded-2xl border border-line bg-bg p-6 text-fg backdrop:bg-black/40"
	oncancel={(e) => running && e.preventDefault()}
>
	<div class="flex flex-col gap-5">
		<div>
			<h2 class="text-[17px] font-medium">{t('backups.file.title', { db })}</h2>
			<p class="text-sm text-muted">{t('backups.file.lead', { db })}</p>
		</div>

		{#if sent === null}
			<label
				class="flex cursor-pointer flex-col items-center gap-2 rounded-2xl border border-dashed px-4 py-6 text-center transition {dragging
					? 'border-fg bg-hover'
					: 'border-line hover:bg-hover'}"
				ondragover={(e) => {
					e.preventDefault();
					dragging = true;
				}}
				ondragleave={() => (dragging = false)}
				ondrop={drop}
			>
				<span class="grid size-10 place-items-center rounded-xl bg-selected"><Upload size={20} strokeWidth={1.75} /></span>
				{#if file}
					<span class="max-w-full truncate font-mono text-[13px]">{file.name}</span>
					<span class="text-sm text-muted">{bytes(file.size)} · {t('backups.file.change')}</span>
				{:else}
					<span class="text-[15px]">{t('backups.file.choose')}</span>
					<span class="text-sm text-muted">{t('backups.file.types', { types })}</span>
				{/if}
				<input type="file" class="sr-only" onchange={(e) => pick(e.currentTarget.files?.[0])} />
			</label>
			<p class="text-sm text-muted">{redis ? t('backups.file.redis') : t(engine === 'postgres' ? 'backups.file.adaptPostgres' : 'backups.file.adaptMariadb')}</p>
			<ol class="flex flex-col gap-3 text-sm">
				{#each [t('backups.file.step.upload'), t('backups.file.step.check', { days }), t('backups.step.safety', { db }), apps.length && redis ? t('backups.step.stopBoth', { apps: list(apps), n: apps.length, db }) : apps.length ? t('backups.step.stopApps', { apps: list(apps), n: apps.length }) : redis ? t('backups.step.stopDb', { db }) : t('backups.step.noApps'), t('backups.file.step.replace', { db }), t('backups.step.start')] as step, i (i)}
					<li class="flex gap-3">
						<span class="grid size-6 shrink-0 place-items-center rounded-full bg-selected text-xs">{i + 1}</span>
						<span>{step}</span>
					</li>
				{/each}
			</ol>
		{:else}
			<div class="flex flex-col gap-2">
				<p class="flex min-w-0 items-center justify-between gap-3 text-sm">
					<span class="truncate font-mono text-[13px]">{file?.name}</span>
					<span class="shrink-0 text-muted tabular-nums">{percent}%</span>
				</p>
				<div class="h-2 overflow-hidden rounded-full bg-selected" role="progressbar" aria-valuenow={percent} aria-valuemin={0} aria-valuemax={100}>
					<div class="h-full rounded-full bg-accent transition-[width]" style="width: {percent}%"></div>
				</div>
				<p class="flex items-center gap-2 text-sm text-muted">
					{#if running}<LoaderCircle size={14} class="shrink-0 animate-spin" />{/if}
					{retrying ? t('backups.file.retrying') : t('backups.file.progress', { done: bytes(sent), total: bytes(file?.size ?? 0) })}
				</p>
				{#if !stopped}<p class="text-sm text-muted">{t('backups.file.keepOpen')}</p>{/if}
			</div>
		{/if}

		<ErrorText message={error} />

		<div class="flex flex-wrap gap-3">
			{#if sent === null}
				<Button onclick={start} disabled={!file}><Upload size={16} strokeWidth={1.75} />{t('backups.file.start')}</Button>
				<Button kind="quiet" onclick={() => dialog?.close()}>{t('common.cancel')}</Button>
			{:else if stopped}
				<Button onclick={start}>{t('backups.file.resume')}</Button>
				<Button kind="quiet" onclick={stop}>{t('backups.file.stop')}</Button>
			{:else}
				<Button kind="quiet" onclick={stop}>{t('backups.file.stop')}</Button>
			{/if}
		</div>
	</div>
</dialog>
