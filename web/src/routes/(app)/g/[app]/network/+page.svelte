<script lang="ts">
	import { Plus, X } from '@lucide/svelte';
	import { onMount, untrack } from 'svelte';
	import { api } from '$lib/api';
	import { messageOf } from '$lib/errors';
	import { address, game, gameState, savePorts, type Allocation, type GamePort } from '$lib/games.svelte';
	import { t } from '$lib/i18n';
	import { freePorts, portLabel } from '$lib/ports';
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
		const list: Row[] = g.ports.map((p) => {
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
	const optionsFor = (r: Row) => known.filter((a) => a.id === r.id || !rows.some((o) => o.id === a.id));
	const spare = $derived(freePorts(pool).filter((a) => !g.ports.some((p) => p.id === a.id) && !rows.some((r) => r.id === a.id)));

	const dirty = $derived(rows.length !== g.ports.length || rows.some((r) => r.saved?.id !== r.id));

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
			for (const r of rows) if (r.env) variables[r.env] = r.id;
			await savePorts(g.id, {
				primary: rows.find((r) => r.key === 'primary')?.id ?? 0,
				variables,
				extra: rows.filter((r) => r.key !== 'primary' && !r.env).map((r) => r.id)
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
			{#each rows as r (r.key)}
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
			{/each}
		</ul>
		{#if poolOpen}
			<p class="text-sm text-muted">{t('network.noFree')}</p>
			<PoolAdd onadded={poolAdded} oncancel={() => (poolOpen = false)} />
		{/if}
		<ErrorText message={error} />
		<div class="flex flex-wrap items-center gap-3">
			<Button type="submit" {busy} disabled={!dirty || !stopped}>{t('network.save')}</Button>
			<Button kind="secondary" type="button" disabled={!stopped || busy || rows.length >= 16} onclick={add}><Plus size={16} strokeWidth={1.75} />{t('network.add')}</Button>
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
