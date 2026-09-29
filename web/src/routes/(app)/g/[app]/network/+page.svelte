<script lang="ts">
	import { Plus, X } from '@lucide/svelte';
	import { onMount, untrack } from 'svelte';
	import { api } from '$lib/api';
	import { messageOf } from '$lib/errors';
	import { address, game, gameState, savePorts, type Allocation, type GamePort } from '$lib/games.svelte';
	import { t } from '$lib/i18n';
	import { freePorts, portLabel, runFrom, runStarts } from '$lib/ports';
	import type { NodeInfo } from '$lib/server.svelte';
	import Button from '$lib/ui/Button.svelte';
	import ErrorText from '$lib/ui/ErrorText.svelte';
	import PoolAdd from '$lib/ui/PoolAdd.svelte';
	import SFTPAccess from '$lib/ui/SFTPAccess.svelte';

	const g = $derived(game.info!);
	const stopped = $derived(gameState(g) === 'stopped');
	let node = $state<NodeInfo | null>(null);
	let pool = $state<Allocation[]>([]);

	// One row per port the server holds: the one players join on, the ones
	// the egg's variables hold, and extras. id is the allocation the row
	// will have once saved.
	type Row = { key: string; env: string; saved: GamePort | null; id: number };
	let rows = $state<Row[]>([]);
	let filled = $state(false);
	let busy = $state(false);
	let error = $state('');
	let saved = $state(false);
	let poolOpen = $state(false);
	let fresh = 0;

	function fill() {
		// The ports after the game port come with it and have no row of their own.
		const list: Row[] = g.ports
			.filter((p) => !p.offset)
			.map((p) => {
			const env = p.default ? '' : (p.used_by[0]?.env ?? '');
				return { key: p.default ? 'primary' : env || `extra:${p.id}`, env, saved: p, id: p.id };
			});
		// The port players join on first, then the ones with a use, then extras.
		const rank = (r: Row) => (r.key === 'primary' ? 0 : r.env ? 1 : 2);
		rows = list.sort((a, b) => rank(a) - rank(b));
	}

	async function loadPool() {
		pool = await api<Allocation[]>('GET', '/nodes/1/allocations').catch(() => pool);
	}

	onMount(() => {
		api<NodeInfo>('GET', '/nodes/1')
			.then((n) => (node = n))
			.catch(() => {});
		loadPool();
	});

	$effect(() => {
		g.id;
		untrack(() => {
			if (!filled) fill();
			filled = true;
		});
	});

	// Ports a row can take: what the server holds and what is free, less
	// what other rows already chose.
	// Keyed by id: right after a save the server's ports are also in the
	// pool as it was read before.
	const known = $derived(
		[...new Map([...freePorts(pool), ...g.ports].map((a) => [a.id, { id: a.id, ip: a.ip, port: a.port }])).values()].sort((a, b) => a.port - b.port)
	);
	const block = $derived(g.block ?? 1);
	const primary = $derived(rows.find((r) => r.key === 'primary'));
	const isExtra = (r: Row) => r.key.startsWith('extra:') || r.key.startsWith('new:');
	// A game with ports after its own takes them with the game port, from
	// what is free or already the server's, keeping clear of the other rows.
	const usableForRun = (a: { id: number }) => !rows.some((o) => o !== primary && !isExtra(o) && o.id === a.id);
	const run = $derived(block > 1 && primary ? (runFrom(known, primary.id, block, usableForRun) ?? []) : []);
	const runIds = $derived(new Set(run.map((a) => a.id)));
	// Extras that lie on the run are part of it now.
	const shownRows = $derived(rows.filter((r) => !(isExtra(r) && runIds.has(r.id))));
	const starts = $derived(block > 1 ? runStarts(known, block, usableForRun) : []);
	const optionsFor = (r: Row) => {
		if (r.key === 'primary' && block > 1) {
			// The current port stays in the list so the box shows it, even
			// when its run is broken.
			return known.filter((a) => a.id === r.id || starts.some((s) => s.id === a.id));
		}
		return known.filter((a) => a.id === r.id || (!rows.some((o) => o.id === a.id) && !runIds.has(a.id)));
	};
	const spare = $derived(freePorts(pool).filter((a) => !g.ports.some((p) => p.id === a.id) && !rows.some((r) => r.id === a.id) && !runIds.has(a.id)));

	const held = $derived(g.ports.filter((p) => !p.offset));
	// A broken run is mended by saving once the game port has a whole run.
	const mends = $derived(!!g.block_broken && run.length === block);
	const dirty = $derived(shownRows.length !== held.length || shownRows.some((r) => r.saved?.id !== r.id) || mends);

	function add() {
		const next = spare[0];
		if (!next) {
			poolOpen = true;
			return;
		}
		rows.push({ key: `new:${fresh++}`, env: '', saved: null, id: next.id });
	}

	async function save() {
		busy = true;
		error = '';
		saved = false;
		try {
			const variables: Record<string, number> = {};
			for (const r of shownRows) if (r.env) variables[r.env] = r.id;
			await savePorts(g.id, {
				primary: rows.find((r) => r.key === 'primary')?.id ?? 0,
				variables,
				extra: shownRows.filter((r) => r.key !== 'primary' && !r.env).map((r) => r.id)
			});
			fill();
			saved = true;
			await loadPool();
		} catch (err) {
			error = messageOf(err);
		} finally {
			busy = false;
		}
	}

	async function poolAdded() {
		poolOpen = false;
		await loadPool();
	}
</script>

<div class="flex max-w-2xl flex-col gap-6">
	<div>
		<h2 class="font-medium">{t('network.title')}</h2>
		<p class="text-sm text-muted">{t('network.lead')}</p>
	</div>
	<form
		class="flex flex-col gap-4"
		onsubmit={(e) => {
			e.preventDefault();
			if (dirty && stopped && !busy) save();
		}}
	>
		<ul class="flex flex-col divide-y divide-line rounded-2xl border border-line">
			{#each shownRows as r (r.key)}
				{@const p = r.saved}
				<li class="flex flex-wrap items-center justify-between gap-x-4 gap-y-2 px-4 py-3">
					<div class="flex min-w-0 flex-col gap-0.5">
						<span class="text-[15px]">
							{#if r.key === 'primary'}{t('game.new.rolePrimary')}{:else if r.env}{game.info?.variables.find((v) => v.env === r.env)?.name ?? r.env}<span
									class="ml-1.5 font-mono text-xs text-muted">· {r.env}</span
								>{:else}{t('network.extra')}{/if}
						</span>
						{#if p}
							<span class="font-mono text-sm text-muted select-all">{address(p)}</span>
						{:else}
							<span class="text-sm text-muted">{t('network.newPort')}</span>
						{/if}
					</div>
					<div class="flex items-center gap-2">
						<select
							class="h-10 w-32 rounded-xl border border-line bg-bg px-3 font-mono text-[15px] disabled:opacity-60"
							aria-label={t('network.change')}
							disabled={!stopped || busy}
							value={r.id}
							onchange={(e) => (r.id = Number(e.currentTarget.value))}
						>
							{#each optionsFor(r) as a (a.id)}<option value={a.id}>{portLabel(a)}</option>{/each}
						</select>
						{#if r.key !== 'primary' && !r.env}
							<button
								type="button"
								class="rounded-lg p-2 text-muted transition hover:bg-hover hover:text-fg disabled:opacity-40"
								aria-label={t('network.remove')}
								title={t('network.remove')}
								disabled={!stopped || busy}
								onclick={() => (rows = rows.filter((o) => o !== r))}><X size={16} strokeWidth={1.75} /></button
							>
						{:else}
							<span class="w-8" aria-hidden="true"></span>
						{/if}
					</div>
				</li>
				{#if r.key === 'primary' && run.length > 1}
					{#each run.slice(1) as f, i (f.id)}
						<li class="flex flex-wrap items-center justify-between gap-x-4 gap-y-2 px-4 py-3">
							<span class="min-w-0 text-[15px] text-muted">{t('network.following', { n: i + 1 })}</span>
							<span class="pr-10 font-mono text-[15px] select-all">{portLabel(f)}</span>
						</li>
					{/each}
				{/if}
			{/each}
		</ul>
		{#if g.block_broken}
			<p class="rounded-xl border border-line bg-panel px-4 py-3 text-sm" role="alert">{t('network.blockBroken', { n: block })}</p>
		{:else if block > 1}
			<p class="text-sm text-muted">{t('network.blockHint', { n: block - 1 })}</p>
		{/if}
		{#if poolOpen}
			<p class="text-sm text-muted">{t('network.noFree')}</p>
			<PoolAdd onadded={poolAdded} oncancel={() => (poolOpen = false)} />
		{/if}
		<ErrorText message={error} />
		<div class="flex flex-wrap items-center gap-3">
			<Button type="submit" {busy} disabled={!dirty || !stopped}>{t('network.save')}</Button>
			<Button kind="secondary" type="button" disabled={!stopped || busy || shownRows.length + block - 1 >= 16} onclick={add}><Plus size={16} strokeWidth={1.75} />{t('network.add')}</Button>
			{#if saved && !dirty}<span class="text-sm text-muted" role="status">{t('network.saved')}</span>{/if}
		</div>
		{#if !stopped}<p class="text-sm text-muted">{t('network.stopFirst')}</p>{/if}
	</form>
	<p class="text-sm text-muted">{t('network.defaultHint')}</p>
	{#if g.ports.some((p) => p.used_by.length)}
		<p class="text-sm text-muted">{t('network.usedByHint')}</p>
	{/if}
	{#if node?.private}
		<p class="text-sm text-muted">
			{t('network.private')}
			<a href="/server" class="underline hover:text-fg">{t('network.privateLink')}</a>
		</p>
	{/if}
	<SFTPAccess id={g.id} />
</div>
