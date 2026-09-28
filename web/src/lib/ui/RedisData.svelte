<script lang="ts">
	import { KeyRound, LoaderCircle, Search } from '@lucide/svelte';
	import { api } from '$lib/api';
	import { has, type Key, type KeyPage, type Value } from '$lib/data';
	import { messageOf } from '$lib/errors';
	import { t, type Key as TextKey } from '$lib/i18n';
	import Button from '$lib/ui/Button.svelte';
	import ErrorText from '$lib/ui/ErrorText.svelte';

	let { db }: { db: string } = $props();

	let pattern = $state('*');
	let keys = $state<Key[]>([]);
	let cursor = $state('0');
	let scanned = $state(false);
	let loading = $state(false);
	let error = $state('');
	let open = $state<Value | null>(null);
	let opening = $state('');

	// A step through the key space can find nothing and still not be at the
	// end; a few steps are taken until something shows up.
	async function scan(from: string) {
		loading = true;
		error = '';
		try {
			let c = from;
			let found = 0;
			for (let step = 0; step < 10; step++) {
				const body = { pattern: pattern.trim() || '*', cursor: c };
				// Reads change nothing: one lost to a dropped connection is asked again.
				const p = await api<KeyPage>('POST', `/apps/${db}/data/keys`, body).catch((err) => {
					if (err instanceof TypeError) return api<KeyPage>('POST', `/apps/${db}/data/keys`, body);
					throw err;
				});
				keys = [...keys, ...p.keys];
				found += p.keys.length;
				c = p.cursor;
				if (c === '0' || found >= 50) break;
			}
			cursor = c;
			scanned = true;
		} catch (err) {
			error = messageOf(err);
		} finally {
			loading = false;
		}
	}
	function find(e?: SubmitEvent) {
		e?.preventDefault();
		keys = [];
		open = null;
		scan('0');
	}
	$effect(() => {
		void db;
		find();
	});

	async function show(k: Key) {
		opening = k.id;
		error = '';
		try {
			open = await api<Value>('POST', `/apps/${db}/data/key`, { id: k.id }).catch((err) => {
				if (err instanceof TypeError) return api<Value>('POST', `/apps/${db}/data/key`, { id: k.id });
				throw err;
			});
		} catch (err) {
			error = messageOf(err);
		} finally {
			opening = '';
		}
	}

	const ttl = (s: number) => (s < 0 ? t('viewer.redis.noExpiry') : t('viewer.redis.expires', { s }));
	const size = (v: Value) => t(v.key.type === 'string' ? 'viewer.redis.chars' : 'viewer.redis.items', { n: v.size });
</script>

<div class="flex flex-col gap-4">
	<form class="flex flex-wrap gap-2" onsubmit={find}>
		<div class="relative min-w-0 flex-1 sm:max-w-sm">
			<Search size={14} class="pointer-events-none absolute top-1/2 left-2.5 -translate-y-1/2 text-muted" />
			<input
				class="h-9 w-full rounded-lg border border-line bg-bg pr-2 pl-8 font-mono text-sm"
				bind:value={pattern}
				aria-label={t('viewer.redis.pattern')}
				placeholder="user:*"
			/>
		</div>
		<Button type="submit" kind="secondary" class="!h-9 !px-4 text-sm" busy={loading}>{t('viewer.redis.find')}</Button>
	</form>
	<p class="-mt-2 text-xs text-muted">{t('viewer.redis.patternHint')}</p>

	<ErrorText message={error} />

	<div class="flex flex-col gap-4 lg:flex-row lg:items-start">
		<div class="flex min-w-0 flex-col gap-2 lg:w-80 lg:shrink-0">
			{#if scanned && keys.length === 0 && !loading}
				<div class="flex flex-col items-start gap-3 rounded-2xl border border-dashed border-line p-5">
					<span class="grid size-10 place-items-center rounded-xl bg-selected"><KeyRound size={20} strokeWidth={1.75} /></span>
					<p class="text-sm text-muted">{cursor === '0' ? t('viewer.redis.none') : t('viewer.redis.noneYet')}</p>
				</div>
			{:else if keys.length}
				<ul class="flex flex-col divide-y divide-line rounded-2xl border border-line">
					{#each keys as k (k.id)}
						<li>
							<button
								type="button"
								class="flex w-full items-center gap-2 px-3 py-2 text-left transition {open?.key.id === k.id ? 'bg-selected' : 'hover:bg-hover'}"
								onclick={() => show(k)}
							>
								<span class="min-w-0 flex-1 truncate font-mono text-[13px]">{k.name}</span>
								{#if opening === k.id}<LoaderCircle size={12} class="animate-spin" />{/if}
								<span class="shrink-0 rounded-md bg-selected px-1.5 py-0.5 text-xs text-muted">{k.type}</span>
							</button>
						</li>
					{/each}
				</ul>
			{/if}
			{#if cursor !== '0' && keys.length}
				<Button kind="quiet" class="self-start !h-8 !px-3 text-sm" busy={loading} onclick={() => scan(cursor)}>{t('viewer.redis.more')}</Button>
			{/if}
		</div>

		{#if open}
			<div class="flex min-w-0 flex-1 flex-col gap-3 rounded-2xl bg-panel p-4">
				<div>
					<p class="font-mono text-[13px] break-all">{open.key.name}</p>
					<p class="text-sm text-muted">{open.key.type} · {size(open)} · {ttl(open.key.ttl)}</p>
				</div>
				{#if open.columns.length === 0}
					<p class="text-sm text-muted">{t('viewer.redis.module', { type: open.key.type })}</p>
				{:else}
					<div class="overflow-x-auto rounded-xl border border-line bg-bg">
						<table class="w-full text-left text-[13px]">
							{#if open.columns.length > 1}
								<thead>
									<tr>{#each open.columns as c (c)}<th class="border-b border-line px-3 py-2 font-medium">{t(`viewer.redis.col.${c}` as TextKey)}</th>{/each}</tr>
								</thead>
							{/if}
							<tbody>
								{#each open.rows as r, i (i)}
									<tr class="border-b border-line last:border-0">
										{#each r as v, c (c)}
											<td class="px-3 py-1.5 align-top font-mono break-words whitespace-pre-wrap {has(open.binary, i, c) ? 'text-muted' : ''}">
												{v}{#if has(open.cut, i, c)}<span class="text-muted">…</span>{/if}
											</td>
										{/each}
									</tr>
								{/each}
							</tbody>
						</table>
					</div>
					{#if open.key.type !== 'string' && open.size > open.rows.length}
						<p class="text-sm text-muted">{t('viewer.redis.first', { shown: open.rows.length, n: open.size })}</p>
					{/if}
					{#if open.binary?.length}<p class="text-sm text-muted">{t('viewer.redis.hex')}</p>{/if}
				{/if}
			</div>
		{/if}
	</div>
</div>
