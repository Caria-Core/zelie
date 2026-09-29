<script lang="ts">
	import {
		ArrowLeft,
		ChevronRight,
		Download,
		Ellipsis,
		File as FileIcon,
		FileArchive,
		FilePlus,
		FileText,
		Folder,
		FolderPlus,
		Link,
		LoaderCircle,
		PackageOpen,
		Pencil,
		Save,
		Trash,
		Upload as UploadIcon,
		X
	} from '@lucide/svelte';
	import { untrack } from 'svelte';
	import { beforeNavigate, goto } from '$app/navigation';
	import { page } from '$app/state';
	import { ApiError } from '$lib/api';
	import { ask } from '$lib/ask.svelte';
	import { bytes } from '$lib/backups';
	import { messageOf } from '$lib/errors';
	import * as files from '$lib/files';
	import { game, gameState } from '$lib/games.svelte';
	import { t } from '$lib/i18n';
	import Button from '$lib/ui/Button.svelte';
	import ErrorText from '$lib/ui/ErrorText.svelte';
	import NameDialog, { type NameRequest } from '$lib/ui/NameDialog.svelte';

	// base is where the tab lives, /g/<id> or /a/<id>. hint is shown in an
	// empty top folder, for an app whose files the user brings.
	let { base, hint = '' }: { base: string; hint?: string } = $props();
	const id = $derived(page.params.app ?? '');
	const running = $derived(game.info ? ['running', 'starting'].includes(gameState(game.info)) : false);
	// The address holds the folder, so the back button and a reload keep
	// their place.
	const path = $derived(files.clean(page.url.searchParams.get('path') ?? ''));

	let listing = $state<files.FileList | null>(null);
	let loading = $state(true);
	let error = $state('');
	let notice = $state('');
	let working = $state('');
	let selected = $state<string[]>([]);
	let menu = $state<string | null>(null);
	let name = $state<NameRequest | null>(null);
	let answer: ((name: string | null) => void) | null = null;
	let dragging = $state(false);

	type Sending = { key: number; name: string; size: number; sent: number; abort: () => void };
	let sending = $state<Sending[]>([]);
	let sendKey = 0;

	// The file open in the editor, if any.
	let editor = $state<{ path: string; text: string; saved: string } | null>(null);
	let saving = $state(false);
	const dirty = $derived(!!editor && editor.text !== editor.saved);

	async function load() {
		const wanted = path;
		loading = true;
		try {
			const out = await files.list(id, wanted);
			if (wanted !== path) return;
			listing = out;
			error = '';
			// Only what is still in the folder stays chosen.
			selected = selected.filter((p) => out.entries.some((e) => files.join(wanted, e.name) === p));
		} catch (err) {
			if (wanted === path) {
				listing = null;
				error = messageOf(err);
			}
		} finally {
			if (wanted === path) loading = false;
		}
	}

	$effect(() => {
		id;
		path;
		untrack(() => {
			selected = [];
			menu = null;
			notice = '';
			load();
		});
	});

	function go(to: string) {
		goto(to ? `${base}/files?path=${encodeURIComponent(to)}` : `${base}/files`);
	}

	const crumbs = $derived(
		path
			.split('/')
			.filter(Boolean)
			.map((part, i, all) => ({ part, path: all.slice(0, i + 1).join('/') }))
	);

	function asName(request: NameRequest): Promise<string | null> {
		return new Promise((resolve) => {
			name = request;
			answer = (n) => {
				name = null;
				answer = null;
				resolve(n);
			};
		});
	}

	// run does what the user asked and refreshes the folder, showing what
	// went wrong.
	async function run(text: string, job: () => Promise<string | void>) {
		working = text;
		error = '';
		notice = '';
		try {
			notice = (await job()) ?? '';
		} catch (err) {
			error = messageOf(err);
		} finally {
			working = '';
		}
		await load();
	}

	async function newFile() {
		const n = await asName({ title: t('files.newFile'), label: t('files.fileName'), value: '', action: t('files.create') });
		if (n === null) return;
		const target = files.join(path, n);
		await run('', async () => {
			await files.write(id, target, '', true);
			editor = { path: target, text: '', saved: '' };
		});
	}

	async function newFolder() {
		const n = await asName({ title: t('files.newFolder'), label: t('files.folderName'), value: '', action: t('files.create') });
		if (n !== null) await run('', () => files.makeFolder(id, files.join(path, n)) as Promise<void>);
	}

	async function renameEntry(e: files.FileEntry) {
		const n = await asName({ title: t('files.rename'), label: t('files.newName'), value: e.name, action: t('files.rename'), hint: t('files.renameHint') });
		if (n === null || n === e.name) return;
		await run('', () => files.rename(id, files.join(path, e.name), n.startsWith('/') ? files.clean(n) : files.join(path, n)) as Promise<void>);
	}

	async function removePaths(paths: string[], label: string) {
		const ok = await ask({
			title: paths.length === 1 ? t('files.deleteOne', { name: label }) : t('files.deleteMany', { n: paths.length }),
			text: t('files.deleteText'),
			action: t('files.delete'),
			danger: true
		});
		if (ok) await run('', () => files.remove(id, paths) as Promise<void>);
	}

	async function compressPaths(paths: string[]) {
		await run(t('files.compressing'), async () => {
			const out = await files.compress(id, path, paths);
			selected = [];
			return t('files.compressed', { name: out.name });
		});
	}

	async function extractEntry(e: files.FileEntry) {
		await run(t('files.extracting'), async () => {
			const out = await files.extract(id, files.join(path, e.name));
			return t('files.extracted', { n: out.entries });
		});
	}

	async function open(e: files.FileEntry) {
		const target = files.join(path, e.name);
		if (e.dir) return go(target);
		if (e.symlink) {
			// A link may lead to a folder; the core follows it only inside the volume.
			try {
				await files.list(id, target);
				return go(target);
			} catch {
				// Not a folder: open it as a file.
			}
		}
		if (e.size > files.editLimit) {
			error = t('files.tooLarge');
			return;
		}
		working = ' ';
		error = '';
		try {
			const text = await files.read(id, target);
			editor = { path: target, text, saved: text };
		} catch (err) {
			error = messageOf(err);
		} finally {
			working = '';
		}
	}

	async function save() {
		if (!editor || saving || !dirty) return;
		saving = true;
		error = '';
		const text = editor.text;
		try {
			await files.write(id, editor.path, text);
			if (editor) editor.saved = text;
			notice = t('files.saved');
		} catch (err) {
			error = messageOf(err);
		} finally {
			saving = false;
		}
	}

	async function leaveEditor(): Promise<boolean> {
		if (!dirty) return true;
		return ask({ title: t('files.leaveTitle'), text: t('files.leaveText', { name: editor!.path.split('/').pop() ?? '' }), action: t('files.leave'), danger: true });
	}

	async function closeEditor() {
		if (!(await leaveEditor())) return;
		editor = null;
		notice = '';
		load();
	}

	// Leaving the page or the folder with changes not saved asks first.
	beforeNavigate((nav) => {
		if (!dirty) return;
		nav.cancel();
		if (nav.willUnload || !nav.to) return;
		const to = nav.to.url;
		leaveEditor().then((ok) => {
			if (ok) {
				editor = null;
				goto(to);
			}
		});
	});

	function keys(e: KeyboardEvent) {
		if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 's' && editor) {
			e.preventDefault();
			save();
		}
		if (e.key === 'Escape') menu = null;
	}

	async function send(list: FileList | File[]) {
		const dir = path;
		for (const file of Array.from(list)) {
			const item: Sending = $state({ key: ++sendKey, name: file.name, size: file.size, sent: 0, abort: () => {} });
			sending.push(item);
			const up = files.upload(id, files.join(dir, file.name), file, (n) => (item.sent = n));
			item.abort = up.abort;
			try {
				await up.promise;
			} catch (err) {
				if (!(err instanceof DOMException && err.name === 'AbortError')) error = messageOf(err);
				if (err instanceof ApiError && err.status === 401) break;
			} finally {
				sending = sending.filter((s) => s.key !== item.key);
			}
		}
		if (dir === path) await load();
	}

	let picker = $state<HTMLInputElement>();

	function iconFor(e: files.FileEntry) {
		if (e.dir) return Folder;
		if (e.symlink) return Link;
		if (files.isArchive(e.name)) return FileArchive;
		if (/\.(txt|md|json|ya?ml|toml|properties|conf|cfg|ini|log|xml|sh|js|py|lua|csv)$/i.test(e.name)) return FileText;
		return FileIcon;
	}

	const entries = $derived(listing?.entries ?? []);
	const allChosen = $derived(entries.length > 0 && selected.length === entries.length);

	function toggle(p: string) {
		selected = selected.includes(p) ? selected.filter((x) => x !== p) : [...selected, p];
	}
</script>

<svelte:window
	onkeydown={keys}
	onclick={() => (menu = null)}
	onbeforeunload={(e) => {
		if (dirty) e.preventDefault();
	}}
/>

{#snippet crumb(label: string, to: string, last: boolean)}
	{#if last}
		<span class="truncate font-medium">{label}</span>
	{:else}
		<a href={to ? `${base}/files?path=${encodeURIComponent(to)}` : `${base}/files`} class="truncate text-muted transition hover:text-fg">{label}</a>
	{/if}
{/snippet}

{#if editor}
	{@const parts = editor.path.split('/')}
	<div class="flex flex-col gap-4">
		<div class="flex flex-wrap items-center justify-between gap-3">
			<div class="flex min-w-0 items-center gap-2">
				<Button kind="quiet" class="!px-3" aria-label={t('files.back')} title={t('files.back')} onclick={closeEditor}><ArrowLeft size={18} strokeWidth={1.75} /></Button>
				<p class="truncate font-mono text-sm">{editor.path}</p>
				{#if dirty}<span class="shrink-0 text-sm text-muted">· {t('files.unsaved')}</span>{/if}
			</div>
			<Button onclick={save} busy={saving} disabled={!dirty}><Save size={16} strokeWidth={1.75} />{t('files.save')}</Button>
		</div>
		{#if running}<p class="rounded-xl border border-line px-4 py-3 text-sm text-muted">{t('files.runningNote')}</p>{/if}
		<ErrorText message={error} />
		{#if notice && !dirty}<p class="text-sm text-muted" role="status">{notice}</p>{/if}
		<textarea
			bind:value={editor.text}
			aria-label={t('files.editorLabel', { name: parts.at(-1) ?? '' })}
			spellcheck="false"
			autocomplete="off"
			autocapitalize="off"
			wrap="off"
			class="h-[min(65vh,42rem)] min-h-64 w-full resize-y rounded-2xl border border-line bg-bg p-4 font-mono text-[13px] leading-relaxed outline-none transition [tab-size:4] focus:border-muted"
		></textarea>
	</div>
{:else}
	<!-- svelte-ignore a11y_no_static_element_interactions -->
	<div
		class="relative flex flex-col gap-4"
		ondragover={(e) => {
			if (e.dataTransfer?.types.includes('Files')) {
				e.preventDefault();
				dragging = true;
			}
		}}
		ondragleave={(e) => {
			if (!e.currentTarget.contains(e.relatedTarget as Node)) dragging = false;
		}}
		ondrop={(e) => {
			e.preventDefault();
			dragging = false;
			if (e.dataTransfer?.files.length) send(e.dataTransfer.files);
		}}
	>
		<div class="flex flex-wrap items-center justify-between gap-3">
			<nav aria-label={t('files.path')} class="flex min-w-0 flex-wrap items-center gap-1 text-[15px]">
				{@render crumb(t('files.home'), '', crumbs.length === 0)}
				{#each crumbs as c, i (c.path)}
					<ChevronRight size={14} class="shrink-0 text-muted" aria-hidden="true" />
					{@render crumb(c.part, c.path, i === crumbs.length - 1)}
				{/each}
			</nav>
			<div class="flex flex-wrap items-center gap-2">
				<Button kind="secondary" onclick={newFile}><FilePlus size={16} strokeWidth={1.75} />{t('files.newFile')}</Button>
				<Button kind="secondary" onclick={newFolder}><FolderPlus size={16} strokeWidth={1.75} />{t('files.newFolder')}</Button>
				<Button kind="secondary" onclick={() => picker?.click()}><UploadIcon size={16} strokeWidth={1.75} />{t('files.upload')}</Button>
				<input
					bind:this={picker}
					type="file"
					multiple
					class="hidden"
					onchange={(e) => {
						const input = e.currentTarget;
						if (input.files?.length) send(input.files);
						input.value = '';
					}}
				/>
			</div>
		</div>

		<ErrorText message={error} />
		{#if notice}<p class="text-sm text-muted" role="status">{notice}</p>{/if}
		{#if working.trim()}
			<p class="flex items-center gap-2 text-sm text-muted" role="status"><LoaderCircle size={16} class="animate-spin" />{working}</p>
		{/if}

		{#each sending as s (s.key)}
			<div class="flex flex-col gap-1.5 rounded-xl border border-line px-4 py-3">
				<div class="flex items-center justify-between gap-3 text-sm">
					<span class="truncate">{t('files.uploading', { name: s.name })}</span>
					<span class="flex shrink-0 items-center gap-2 text-muted">
						{bytes(s.sent)} / {bytes(s.size)}
						<button class="rounded-md p-1 transition hover:bg-hover hover:text-fg" aria-label={t('files.uploadCancel', { name: s.name })} onclick={() => s.abort()}><X size={14} /></button>
					</span>
				</div>
				<div class="h-1.5 overflow-hidden rounded-full bg-selected" role="progressbar" aria-valuemin="0" aria-valuemax="100" aria-valuenow={s.size ? Math.round((s.sent / s.size) * 100) : 100}>
					<div class="h-full rounded-full bg-accent transition-[width]" style="width: {s.size ? (s.sent / s.size) * 100 : 100}%"></div>
				</div>
			</div>
		{/each}

		{#if selected.length}
			<div class="flex flex-wrap items-center justify-between gap-3 rounded-xl bg-selected px-4 py-2.5">
				<span class="text-sm font-medium">{t('files.selected', { n: selected.length })}</span>
				<div class="flex flex-wrap items-center gap-1">
					<Button kind="quiet" class="!h-8 !px-3" onclick={() => compressPaths(selected)}><FileArchive size={16} strokeWidth={1.75} />{t('files.compress')}</Button>
					<Button kind="quiet" class="!h-8 !px-3 hover:!text-danger" onclick={() => removePaths(selected, '')}><Trash size={16} strokeWidth={1.75} />{t('files.delete')}</Button>
					<Button kind="quiet" class="!h-8 !px-3" onclick={() => (selected = [])}>{t('files.clear')}</Button>
				</div>
			</div>
		{/if}

		{#if loading && !listing}
			<p class="text-sm text-muted">{t('files.loading')}</p>
		{:else if listing}
			<div class="rounded-2xl border border-line {dragging ? 'border-dashed !border-muted' : ''}">
				{#if entries.length}
					<div class="hidden grid-cols-[2.25rem_1fr_6rem_11rem_2.25rem] items-center gap-3 border-b border-line px-3 py-2 text-xs text-muted sm:grid">
						<input type="checkbox" aria-label={t('files.selectAll')} checked={allChosen} onchange={() => (selected = allChosen ? [] : entries.map((e) => files.join(path, e.name)))} class="size-4 accent-current" />
						<span>{t('files.name')}</span>
						<span class="text-right">{t('files.size')}</span>
						<span>{t('files.modified')}</span>
						<span></span>
					</div>
					<ul class="divide-y divide-line">
						{#each entries as e (e.name)}
							{@const p = files.join(path, e.name)}
							{@const Icon = iconFor(e)}
							<li class="grid grid-cols-[2.25rem_1fr_2.25rem] items-center gap-3 px-3 py-1.5 transition hover:bg-hover sm:grid-cols-[2.25rem_1fr_6rem_11rem_2.25rem] {selected.includes(p) ? 'bg-selected' : ''}">
								<input type="checkbox" aria-label={t('files.select', { name: e.name })} checked={selected.includes(p)} onchange={() => toggle(p)} class="size-4 accent-current" />
								<button class="flex min-w-0 items-center gap-3 py-1.5 text-left" onclick={() => open(e)}>
									<Icon size={18} strokeWidth={1.75} class="shrink-0 {e.dir ? 'text-fg' : 'text-muted'}" />
									<span class="truncate text-[15px]">{e.name}</span>
									{#if e.symlink && e.target}<span class="hidden truncate text-sm text-muted md:inline">{t('files.linkTo', { target: e.target })}</span>{/if}
								</button>
								<span class="hidden text-right text-sm text-muted tabular-nums sm:block">{e.dir ? '' : bytes(e.size)}</span>
								<span class="hidden text-sm text-muted sm:block">{files.modified(e.modified)}</span>
								<div class="relative">
									<button
										class="rounded-lg p-1.5 text-muted transition hover:bg-line hover:text-fg"
										aria-label={t('files.menu', { name: e.name })}
										aria-haspopup="menu"
										aria-expanded={menu === p}
										onclick={(ev) => {
											ev.stopPropagation();
											menu = menu === p ? null : p;
										}}
									>
										<Ellipsis size={18} strokeWidth={1.75} />
									</button>
									{#if menu === p}
										<!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
										<div role="menu" tabindex="-1" class="absolute top-9 right-0 z-20 w-48 rounded-2xl border border-line bg-bg p-1.5 shadow-lg" onclick={(ev) => ev.stopPropagation()} onkeydown={() => {}}>
											{#if !e.dir}
												<button role="menuitem" class="flex w-full items-center gap-2.5 rounded-xl px-3 py-2 text-left text-[15px] transition hover:bg-hover" onclick={() => ((menu = null), open(e))}><FileText size={16} strokeWidth={1.75} />{t('files.edit')}</button>
											{/if}
											<button role="menuitem" class="flex w-full items-center gap-2.5 rounded-xl px-3 py-2 text-left text-[15px] transition hover:bg-hover" onclick={() => ((menu = null), renameEntry(e))}><Pencil size={16} strokeWidth={1.75} />{t('files.rename')}</button>
											{#if !e.dir}
												<a role="menuitem" href={files.downloadUrl(id, p)} download class="flex w-full items-center gap-2.5 rounded-xl px-3 py-2 text-left text-[15px] transition hover:bg-hover" onclick={() => (menu = null)}><Download size={16} strokeWidth={1.75} />{t('files.download')}</a>
											{/if}
											<button role="menuitem" class="flex w-full items-center gap-2.5 rounded-xl px-3 py-2 text-left text-[15px] transition hover:bg-hover" onclick={() => ((menu = null), compressPaths([p]))}><FileArchive size={16} strokeWidth={1.75} />{t('files.compress')}</button>
											{#if !e.dir && files.isArchive(e.name)}
												<button role="menuitem" class="flex w-full items-center gap-2.5 rounded-xl px-3 py-2 text-left text-[15px] transition hover:bg-hover" onclick={() => ((menu = null), extractEntry(e))}><PackageOpen size={16} strokeWidth={1.75} />{t('files.extract')}</button>
											{/if}
											<button role="menuitem" class="flex w-full items-center gap-2.5 rounded-xl px-3 py-2 text-left text-[15px] text-danger transition hover:bg-hover" onclick={() => ((menu = null), removePaths([p], e.name))}><Trash size={16} strokeWidth={1.75} />{t('files.delete')}</button>
										</div>
									{/if}
								</div>
							</li>
						{/each}
					</ul>
				{:else}
					<p class="px-4 py-10 text-center text-sm text-muted">{dragging ? t('files.drop') : hint && !path ? hint : t('files.empty')}</p>
				{/if}
			</div>
			{#if listing.truncated}<p class="text-sm text-muted">{t('files.truncated', { n: entries.length })}</p>{/if}
		{/if}
	</div>
{/if}

<NameDialog request={name} onanswer={(n) => answer?.(n)} />
