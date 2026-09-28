<script lang="ts">
	import { ArrowDown, ArrowUp, ChevronLeft, ChevronRight, Download, Eye, Filter as FilterIcon, LoaderCircle, Search, Table2, X } from '@lucide/svelte';
	import { api } from '$lib/api';
	import { bytes } from '$lib/backups';
	import { sensitive } from '$lib/confirm.svelte';
	import { approx, bare, has, ops, type Filter, type Page, type Query, type Table } from '$lib/data';
	import { messageOf } from '$lib/errors';
	import { t } from '$lib/i18n';
	import Button from '$lib/ui/Button.svelte';
	import ErrorText from '$lib/ui/ErrorText.svelte';

	let { db }: { db: string } = $props();

	let tables = $state<Table[] | null>(null);
	let chosen = $state<Table | null>(null);
	let page = $state<Page | null>(null);
	let error = $state('');
	let loading = $state(false);

	// The query being looked at. Filters are edited in a draft and applied
	// together.
	let search = $state('');
	let filters = $state<Filter[]>([]);
	let draft = $state<Filter[]>([]);
	let editing = $state(false);
	let sort = $state('');
	let desc = $state(false);
	let offset = $state(0);

	async function loadTables() {
		error = '';
		try {
			tables = await api<Table[]>('GET', `/apps/${db}/data/tables`).catch((err) => {
				if (err instanceof TypeError) return api<Table[]>('GET', `/apps/${db}/data/tables`);
				throw err;
			});
			if (!chosen && tables.length) pick(tables[0]);
		} catch (err) {
			tables = [];
			error = messageOf(err);
		}
	}
	$effect(() => {
		void db;
		loadTables();
	});

	const query = (): Query => ({
		schema: chosen!.schema,
		table: chosen!.name,
		filters,
		search: search.trim() || undefined,
		sort: sort || undefined,
		desc,
		offset
	});

	// Reads change nothing, so one lost to a dropped connection is simply
	// asked again.
	async function read<T>(path: string, body: unknown): Promise<T> {
		try {
			return await api<T>('POST', path, body);
		} catch (err) {
			if (!(err instanceof TypeError)) throw err;
			return api<T>('POST', path, body);
		}
	}

	let asked = 0;
	async function load() {
		if (!chosen) return;
		const mine = ++asked;
		loading = true;
		error = '';
		try {
			const p = await read<Page>(`/apps/${db}/data/rows`, query());
			if (mine === asked) page = p;
		} catch (err) {
			if (mine === asked) {
				page = null;
				error = messageOf(err);
			}
		} finally {
			if (mine === asked) loading = false;
		}
	}

	function pick(tb: Table) {
		chosen = tb;
		search = '';
		filters = [];
		editing = false;
		sort = '';
		desc = false;
		offset = 0;
		page = null;
		load();
	}

	function sortBy(col: string) {
		if (sort === col) desc = !desc;
		else [sort, desc] = [col, false];
		offset = 0;
		load();
	}

	function openFilters() {
		draft = filters.length ? filters.map((f) => ({ ...f })) : [{ column: chosen!.columns[0]?.name ?? '', op: 'contains', value: '' }];
		editing = true;
	}
	function applyFilters(e?: SubmitEvent) {
		e?.preventDefault();
		filters = draft.filter((f) => f.column && (bare.includes(f.op) || (f.value ?? '') !== ''));
		editing = false;
		offset = 0;
		load();
	}
	function clearFilters() {
		filters = [];
		draft = [];
		editing = false;
		offset = 0;
		load();
	}
	function find(e: SubmitEvent) {
		e.preventDefault();
		offset = 0;
		load();
	}
	const move = (by: number) => {
		offset = Math.max(0, offset + by);
		load();
	};

	async function exportCSV() {
		try {
			const { url } = await sensitive(() => api<{ url: string }>('POST', `/apps/${db}/data/export`, { ...query(), offset: 0 }));
			const a = document.createElement('a');
			a.href = url;
			a.download = '';
			a.click();
		} catch (err) {
			error = messageOf(err);
		}
	}

	// A row opened on its own: read again in full when the table has a key
	// to find it by.
	let dialog = $state<HTMLDialogElement>();
	let row = $state<{ values: (string | null)[]; cut: boolean[]; full: boolean } | null>(null);
	const keys = $derived(chosen?.columns.filter((c) => c.key) ?? []);
	async function openRow(i: number) {
		const values = page!.rows[i];
		const cut = values.map((_, c) => has(page!.cut, i, c));
		row = { values, cut, full: !cut.some(Boolean) };
		dialog?.showModal();
		if (row.full || !keys.length) return;
		const at = (name: string) => values[page!.columns.indexOf(name)];
		if (keys.some((k) => at(k.name) === null)) return;
		try {
			const p = await api<Page>('POST', `/apps/${db}/data/rows`, {
				schema: chosen!.schema,
				table: chosen!.name,
				full: true,
				filters: keys.map((k) => ({ column: k.name, op: 'eq', value: at(k.name)! }))
			});
			if (p.rows.length === 1) row = { values: p.rows[0], cut: p.rows[0].map((_, c) => has(p.cut, 0, c)), full: true };
		} catch {
			/* the cut values stay, marked */
		}
	}

	const schemas = $derived(new Set(tables?.map((tb) => tb.schema) ?? []));
	const label = (tb: Table) => (schemas.size > 1 ? `${tb.schema}.${tb.name}` : tb.name);
	const rowsLabel = (tb: Table) => (tb.view ? t('viewer.view') : tb.rows < 0 ? t('viewer.rowsUnknown') : t('viewer.rows', { count: tb.rows, n: approx(tb.rows) }));
	const colType = (name: string) => chosen?.columns.find((c) => c.name === name);
	const input = 'h-9 rounded-lg border border-line bg-bg px-2 text-sm';
</script>

{#if tables && tables.length === 0 && !error}
	<div class="flex flex-col items-start gap-3 rounded-2xl border border-dashed border-line p-5">
		<span class="grid size-10 place-items-center rounded-xl bg-selected"><Table2 size={20} strokeWidth={1.75} /></span>
		<p class="text-[15px]">{t('viewer.noTables')}</p>
		<p class="text-sm text-muted">{t('viewer.noTablesHint')}</p>
	</div>
{:else}
	<div class="flex flex-col gap-4 lg:flex-row lg:items-start">
		<!-- Tables: a list beside the rows, a menu above them on a phone. -->
		<nav class="hidden w-56 shrink-0 flex-col gap-0.5 lg:flex" aria-label={t('viewer.tables')}>
			{#if !tables}
				<p class="flex items-center gap-2 px-3 text-sm text-muted"><LoaderCircle size={14} class="animate-spin" />{t('viewer.loading')}</p>
			{/if}
			{#each tables ?? [] as tb (tb.schema + '.' + tb.name)}
				<button
					type="button"
					onclick={() => pick(tb)}
					class="flex flex-col rounded-lg px-3 py-1.5 text-left transition {chosen === tb ? 'bg-selected' : 'hover:bg-hover'}"
					aria-current={chosen === tb ? 'true' : undefined}
				>
					<span class="truncate font-mono text-[13px]">{label(tb)}</span>
					<span class="text-xs text-muted">{rowsLabel(tb)}</span>
				</button>
			{/each}
		</nav>
		{#if tables?.length}
			<select
				class="{input} lg:hidden"
				aria-label={t('viewer.tables')}
				value={chosen ? chosen.schema + '.' + chosen.name : ''}
				onchange={(e) => pick(tables!.find((tb) => tb.schema + '.' + tb.name === e.currentTarget.value)!)}
			>
				{#each tables as tb (tb.schema + '.' + tb.name)}<option value={tb.schema + '.' + tb.name}>{label(tb)} · {rowsLabel(tb)}</option>{/each}
			</select>
		{/if}

		<div class="flex min-w-0 flex-1 flex-col gap-3">
			{#if chosen}
				<div class="flex flex-wrap items-center justify-between gap-2">
					<p class="min-w-0 text-sm text-muted">
						<span class="font-mono text-[13px] text-fg">{label(chosen)}</span> · {rowsLabel(chosen)}{#if !chosen.view && chosen.bytes > 0}{' '}·
							{bytes(chosen.bytes)}{/if}
					</p>
					<div class="flex flex-wrap gap-2">
						<form class="relative" onsubmit={find}>
							<Search size={14} class="pointer-events-none absolute top-1/2 left-2.5 -translate-y-1/2 text-muted" />
							<input class="{input} w-44 pl-8" type="search" bind:value={search} placeholder={t('viewer.search')} aria-label={t('viewer.search')} />
						</form>
						<Button kind="secondary" class="!h-9 !px-3 text-sm" onclick={openFilters}
							><FilterIcon size={14} />{filters.length ? t('viewer.filtersOn', { n: filters.length }) : t('viewer.filter')}</Button
						>
						<Button kind="quiet" class="!h-9 !px-3 text-sm" onclick={exportCSV} title={t('viewer.exportHint')}><Download size={14} />{t('viewer.export')}</Button>
					</div>
				</div>

				{#if editing}
					<form class="flex flex-col gap-2 rounded-2xl bg-panel p-4" onsubmit={applyFilters}>
						{#each draft as f, i (i)}
							<div class="flex flex-wrap items-center gap-2">
								<select class="{input} min-w-0 flex-1 font-mono sm:flex-none" bind:value={f.column} aria-label={t('viewer.column')}>
									{#each chosen.columns as c (c.name)}<option value={c.name}>{c.name}</option>{/each}
								</select>
								<select class={input} bind:value={f.op} aria-label={t('viewer.condition')}>
									{#each ops as op (op)}<option value={op}>{t(`viewer.op.${op}`)}</option>{/each}
								</select>
								{#if !bare.includes(f.op)}
									<input class="{input} min-w-0 flex-1" bind:value={f.value} aria-label={t('viewer.value')} />
								{/if}
								<button
									type="button"
									class="grid size-8 place-items-center rounded-full text-muted hover:bg-hover hover:text-fg"
									aria-label={t('viewer.removeFilter')}
									onclick={() => (draft = draft.filter((_, j) => j !== i))}><X size={15} /></button
								>
							</div>
						{/each}
						<div class="flex flex-wrap gap-2 pt-1">
							<Button type="submit" class="!h-9 !px-4 text-sm">{t('viewer.apply')}</Button>
							<Button
								kind="secondary"
								class="!h-9 !px-4 text-sm"
								onclick={() => (draft = [...draft, { column: chosen!.columns[0]?.name ?? '', op: 'contains', value: '' }])}>{t('viewer.addFilter')}</Button
							>
							{#if filters.length}<Button kind="quiet" class="!h-9 !px-3 text-sm" onclick={clearFilters}>{t('viewer.clearFilters')}</Button>{/if}
						</div>
						<p class="text-xs text-muted">{t('viewer.filterHint')}</p>
					</form>
				{/if}

				<ErrorText message={error} />

				{#if page}
					<div class="overflow-x-auto rounded-2xl border border-line {loading ? 'opacity-60' : ''}">
						<table class="w-full text-left text-[13px]">
							<thead class="bg-panel">
								<tr>
									{#each page.columns as col (col)}
										<th class="border-b border-line px-3 py-2 font-medium whitespace-nowrap">
											<button type="button" class="inline-flex items-center gap-1 hover:text-fg" onclick={() => sortBy(col)} title={colType(col)?.type}>
												<span class="font-mono">{col}</span>
												{#if colType(col)?.key}<span class="text-xs text-muted">{t('viewer.key')}</span>{/if}
												{#if sort === col}{#if desc}<ArrowDown size={12} />{:else}<ArrowUp size={12} />{/if}{/if}
											</button>
										</th>
									{/each}
									<th class="w-8 border-b border-line"></th>
								</tr>
							</thead>
							<tbody>
								{#each page.rows as r, i (i)}
									<tr class="border-b border-line last:border-0 hover:bg-hover">
										{#each r as v, c (c)}
											<td class="max-w-72 truncate px-3 py-1.5 font-mono">
												{#if v === null}<span class="text-muted italic">NULL</span>{:else}{v}{#if has(page.cut, i, c)}<span class="text-muted">…</span>{/if}{/if}
											</td>
										{/each}
										<td class="px-1">
											<button
												type="button"
												class="grid size-7 place-items-center rounded-full text-muted hover:bg-selected hover:text-fg"
												aria-label={t('viewer.openRow')}
												title={t('viewer.openRow')}
												onclick={() => openRow(i)}><Eye size={14} /></button
											>
										</td>
									</tr>
								{/each}
							</tbody>
						</table>
						{#if page.rows.length === 0}
							<p class="px-4 py-6 text-center text-sm text-muted">{filters.length || search.trim() ? t('viewer.noMatch') : t('viewer.empty')}</p>
						{/if}
					</div>
					<div class="flex items-center justify-between gap-3 text-sm text-muted">
						<span>
							{#if page.rows.length}{t('viewer.showing', { from: offset + 1, to: offset + page.rows.length })}{/if}
							{#if loading}<LoaderCircle size={14} class="ml-1 inline animate-spin" />{/if}
						</span>
						<div class="flex gap-1">
							<Button kind="quiet" class="!size-8 !px-0" disabled={offset === 0 || loading} aria-label={t('viewer.previous')} onclick={() => move(-50)}
								><ChevronLeft size={16} /></Button
							>
							<Button kind="quiet" class="!size-8 !px-0" disabled={!page.more || loading} aria-label={t('viewer.next')} onclick={() => move(50)}
								><ChevronRight size={16} /></Button
							>
						</div>
					</div>
				{:else if loading}
					<p class="flex items-center gap-2 text-sm text-muted"><LoaderCircle size={14} class="animate-spin" />{t('viewer.loading')}</p>
				{/if}
			{/if}
		</div>
	</div>
{/if}

<dialog
	bind:this={dialog}
	class="m-auto w-[min(40rem,calc(100vw-2rem))] rounded-2xl border border-line bg-bg p-6 text-fg backdrop:bg-black/40"
	onclose={() => (row = null)}
>
	{#if row && page}
		<div class="flex flex-col gap-4">
			<div class="flex items-start justify-between gap-3">
				<h2 class="text-[17px] font-medium">{t('viewer.rowTitle', { table: chosen ? label(chosen) : '' })}</h2>
				<button type="button" class="grid size-8 place-items-center rounded-full text-muted hover:bg-hover hover:text-fg" aria-label={t('common.close')} onclick={() => dialog?.close()}
					><X size={16} /></button
				>
			</div>
			<dl class="flex max-h-[60vh] flex-col gap-3 overflow-y-auto">
				{#each page.columns as col, c (col)}
					<div class="flex flex-col gap-1">
						<dt class="flex items-baseline gap-2 text-sm">
							<span class="font-mono text-[13px]">{col}</span><span class="text-xs text-muted">{colType(col)?.type}</span>
						</dt>
						<dd class="rounded-lg bg-panel px-3 py-2 font-mono text-[13px] break-words whitespace-pre-wrap">
							{#if row.values[c] === null}<span class="text-muted italic">NULL</span>{:else}{row.values[c]}{#if row.cut[c]}<span class="text-muted">…</span>{/if}{/if}
						</dd>
					</div>
				{/each}
			</dl>
			{#if row.cut.some(Boolean)}
				<p class="text-sm text-muted">{keys.length ? t('viewer.cutLong') : t('viewer.cutNoKey')}</p>
			{/if}
		</div>
	{/if}
</dialog>
