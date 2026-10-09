<script lang="ts">
	import { ask } from '$lib/ask.svelte';
	import { HardDrive, Plus, Trash } from '@lucide/svelte';
	import { api } from '$lib/api';
	import { engineLabel } from '$lib/apps.svelte';
	import { sensitive } from '$lib/confirm.svelte';
	import { current, load, restart } from '$lib/current.svelte';
	import { Cancelled, messageOf } from '$lib/errors';
	import { ago } from '$lib/format';
	import { host, loadHost, megabytes } from '$lib/host.svelte';
	import { t } from '$lib/i18n';
	import { defaultSize, sizeSteps, type Volume } from '$lib/volumes';
	import Button from '$lib/ui/Button.svelte';
	import ErrorText from '$lib/ui/ErrorText.svelte';
	import Field from '$lib/ui/Field.svelte';

	const app = $derived(current.app!);
	const running = $derived(app.state === 'running');
	// A database has the one volume its engine keeps its files in.
	const database = $derived(!!app.engine);
	let volumes = $state<Volume[] | null>(null);
	let error = $state('');
	let busy = $state(false);
	// Set after a change the running app only sees once it starts again.
	let pending = $state(false);

	let path = $state('');
	let size = $state(defaultSize);

	const diskMB = $derived(host.info ? host.info.disk_bytes / 2 ** 20 : 102400);

	// A database's files from before an upgrade, until they are removed.
	type Kept = { id: number; version: string; used_bytes: number | null; kept_at: string };
	let kept = $state<Kept[]>([]);

	async function refresh(id: string) {
		volumes = await api<Volume[]>('GET', `/apps/${id}/volumes`).catch((err) => {
			error = messageOf(err);
			return [];
		});
		if (current.app?.engine) kept = await api<Kept[]>('GET', `/apps/${id}/kept-volumes`).catch(() => []);
	}

	async function removeKept(k: Kept) {
		const v = { engine: engineLabel[app.engine!], version: k.version };
		if (!(await ask({ title: t('db.keptConfirm', v), text: t('db.keptConfirmText'), action: t('db.keptRemove'), danger: true }))) return;
		act(async () => {
			try {
				await sensitive(() => api('DELETE', `/apps/${app.id}/kept-volumes/${k.id}`));
			} catch (err) {
				if (!(err instanceof Cancelled)) throw err;
			}
		});
	}

	// Sizes are measured about once a minute; the page follows along.
	$effect(() => {
		const id = app.id;
		loadHost();
		refresh(id);
		const timer = setInterval(() => document.visibilityState === 'visible' && refresh(id), 30000);
		return () => clearInterval(timer);
	});

	async function act(fn: () => Promise<unknown>) {
		busy = true;
		error = '';
		try {
			await fn();
			await refresh(app.id);
		} catch (err) {
			error = messageOf(err);
		} finally {
			busy = false;
		}
	}

	const add = (e: SubmitEvent) => {
		e.preventDefault();
		act(async () => {
			await api('POST', `/apps/${app.id}/volumes`, { path: path.trim(), limit_mb: size });
			path = '';
			size = defaultSize;
			pending = running;
		});
	};

	const setLimit = (v: Volume, limit: number) =>
		act(async () => {
			await api('PATCH', `/apps/${app.id}/volumes/${v.id}`, { limit_mb: limit });
			await load(app.id);
		});

	// A limit below what the volume holds makes Zelie stop the app, so that
	// one is asked about first.
	async function chooseLimit(v: Volume, select: HTMLSelectElement) {
		const limit = Number(select.value);
		if (v.used_bytes !== null && v.used_bytes > limit * 2 ** 20) {
			const ok = await ask({
				title: t('storage.shrinkConfirm', { path: v.path, limit: megabytes(limit) }),
				text: t('storage.shrinkConfirmText'),
				action: t('storage.shrink'),
				danger: true
			});
			if (!ok) {
				select.value = String(v.limit_mb);
				return;
			}
		}
		setLimit(v, limit);
	}

	async function remove(v: Volume) {
		if (!(await ask({ title: t('storage.deleteConfirm', { path: v.path }), text: t('storage.deleteConfirmText'), action: t('storage.delete'), danger: true }))) return;
		act(() => api('DELETE', `/apps/${app.id}/volumes/${v.id}`));
	}

	const share = (v: Volume) => (v.used_bytes === null ? 0 : v.used_bytes / 2 ** 20 / v.limit_mb);
	const pct = (x: number) => `${Math.min(100, x * 100)}%`;
</script>

<div class="flex max-w-xl flex-col gap-8">
	<div>
		<h2 class="font-medium">{t('storage.title')}</h2>
		<p class="text-sm text-muted">{database ? t('db.storageLead') : t('storage.lead')}</p>
	</div>

	{#if volumes && volumes.length === 0}
		<div class="flex flex-col items-start gap-3 rounded-2xl border border-dashed border-line p-5">
			<span class="grid size-10 place-items-center rounded-xl bg-selected"><HardDrive size={20} strokeWidth={1.75} /></span>
			<p class="text-[15px]">{t('storage.empty')}</p>
			<p class="text-sm text-muted">{t('storage.emptyHint')}</p>
		</div>
	{:else if volumes}
		<ul class="flex flex-col gap-3">
			{#each volumes as v (v.id)}
				{@const s = share(v)}
				<li class="flex flex-col gap-3 rounded-2xl border border-line p-4">
					<div class="flex items-start justify-between gap-3">
						<div class="flex min-w-0 items-center gap-3">
							<span class="grid size-9 shrink-0 place-items-center rounded-xl bg-selected"><HardDrive size={18} strokeWidth={1.75} /></span>
							<div class="min-w-0">
								<p class="truncate font-mono text-[15px]">{v.path}</p>
								<p class="text-sm text-muted">{t('storage.created', { when: ago(v.created_at) })}</p>
							</div>
						</div>
						{#if !database}
							<Button
								kind="quiet"
								class="!h-9 !px-2 hover:!text-danger"
								disabled={busy || running}
								title={running ? t('storage.stopFirst') : t('storage.delete')}
								aria-label={t('storage.delete')}
								onclick={() => remove(v)}><Trash size={16} /></Button
							>
						{/if}
					</div>
					<div class="flex flex-col gap-2">
						<div class="relative h-2 overflow-hidden rounded-full bg-selected" aria-hidden="true">
							<div
								class="absolute inset-y-0 left-0 {s > 1 ? 'bg-danger' : s >= 0.9 ? 'bg-warn' : 'bg-fg'}"
								style:width={pct(s)}
							></div>
						</div>
						<div class="flex flex-wrap items-center justify-between gap-x-4 gap-y-2 text-sm">
							<span class={s > 1 ? 'text-danger' : s >= 0.9 ? 'text-warn' : 'text-muted'}>
								{#if v.used_bytes === null}
									{t('storage.notMeasured')}
								{:else}
									{t('storage.used', { used: megabytes(v.used_bytes / 2 ** 20), limit: megabytes(v.limit_mb) })}
								{/if}
							</span>
							<label class="flex items-center gap-2 text-muted">
								{t('storage.limit')}
								<select
									class="h-8 rounded-lg border border-line bg-bg px-2 text-fg"
									value={v.limit_mb}
									disabled={busy}
									onchange={(e) => chooseLimit(v, e.currentTarget)}
								>
									{#each sizeSteps(diskMB, v.limit_mb) as mb (mb)}<option value={mb}>{megabytes(mb)}</option>{/each}
								</select>
							</label>
						</div>
						{#if s > 1}
							<p class="text-sm text-danger">{t('storage.over')}</p>
						{:else if s >= 0.9}
							<p class="text-sm text-warn">{t('storage.nearly')}</p>
						{/if}
					</div>
				</li>
			{/each}
		</ul>
		{#if running && !database}<p class="text-sm text-muted">{t('storage.stopFirst')}</p>{/if}
	{/if}

	{#if kept.length > 0}
		<div class="flex flex-col gap-3">
			<h3 class="font-medium">{t('db.kept')}</h3>
			<ul class="flex flex-col gap-3">
				{#each kept as k (k.id)}
					<li class="flex items-center justify-between gap-3 rounded-2xl border border-dashed border-line p-4">
						<div class="flex min-w-0 items-center gap-3">
							<span class="grid size-9 shrink-0 place-items-center rounded-xl bg-selected"><HardDrive size={18} strokeWidth={1.75} /></span>
							<div class="min-w-0">
								<p class="truncate text-[15px]">{t('db.keptItem', { engine: engineLabel[app.engine!], version: k.version })}</p>
								<p class="text-sm text-muted">
									{t('db.keptWhen', { when: ago(k.kept_at) })}{k.used_bytes !== null ? ' · ' + megabytes(k.used_bytes / 2 ** 20) : ''}
								</p>
							</div>
						</div>
						<Button kind="secondary" class="!h-9 !px-3.5 text-sm hover:!text-danger" disabled={busy} onclick={() => removeKept(k)}
							><Trash size={16} />{t('db.keptRemove')}</Button
						>
					</li>
				{/each}
			</ul>
		</div>
	{/if}

	{#if pending}
		<div class="flex flex-wrap items-center gap-3 rounded-xl bg-panel px-4 py-3 text-sm">
			<span class="flex-1">{t('storage.pending')}</span>
			<Button kind="secondary" class="!h-8 !px-3.5 text-sm" onclick={() =>
					act(async () => {
						await restart(app.id);
						pending = false;
					})}
				>{t('app.restart')}</Button
			>
		</div>
	{/if}

	{#if !database}
		<form class="flex flex-col gap-4" onsubmit={add}>
			<h3 class="font-medium">{t('storage.add')}</h3>
			<div class="grid gap-4 sm:grid-cols-[1fr_9rem]">
				<Field label={t('storage.path')} placeholder="/data" required autocomplete="off" bind:value={path} />
				<div class="flex flex-col gap-1.5">
					<label for="volume-size" class="text-sm font-medium">{t('storage.limit')}</label>
					<select id="volume-size" class="h-10 rounded-xl border border-line bg-bg px-3 text-[15px]" bind:value={size}>
						{#each sizeSteps(diskMB) as mb (mb)}<option value={mb}>{megabytes(mb)}</option>{/each}
					</select>
				</div>
			</div>
			<p class="-mt-2 text-sm text-muted">{t('storage.pathHint')}</p>
			<ErrorText message={error} />
			<Button type="submit" {busy} class="self-start"><Plus size={16} />{t('storage.addButton')}</Button>
		</form>
	{:else}
		<ErrorText message={error} />
	{/if}

	<p class="text-sm text-muted">{t('storage.gap')}</p>
</div>
