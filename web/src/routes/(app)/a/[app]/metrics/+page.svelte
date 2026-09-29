<script lang="ts">
	import { Activity } from '@lucide/svelte';
	import { api } from '$lib/api';
	import { current } from '$lib/current.svelte';
	import { megabytes } from '$lib/host.svelte';
	import { t } from '$lib/i18n';
	import Chart from '$lib/ui/Chart.svelte';

	type Point = {
		at: string;
		memory_bytes: number;
		cpu: number;
		rx_bytes: number;
		tx_bytes: number;
		requests: number;
		client_errors: number;
		server_errors: number;
	};
	type Metrics = { step: number; points: Point[]; crashes: number };

	const app = $derived(current.app!);
	let range = $state<'1h' | '24h'>('1h');
	let data = $state<Metrics | null>(null);

	// Readings come once a minute.
	$effect(() => {
		const id = app.id;
		const r = range;
		let stopped = false;
		const get = async () => {
			if (document.visibilityState !== 'visible') return;
			const m = await api<Metrics>('GET', `/apps/${encodeURIComponent(id)}/metrics?range=${r}`).catch(() => null);
			if (!stopped && m) data = m;
		};
		get();
		const timer = setInterval(get, 60000);
		return () => {
			stopped = true;
			clearInterval(timer);
		};
	});

	const points = $derived(data?.points ?? []);
	const times = $derived(points.map((p) => new Date(p.at).getTime()));
	// Per minute, whatever a point covers.
	const perMin = $derived((data?.step ?? 60) / 60);
	const totals = $derived.by(() => {
		let req = 0,
			fails = 0,
			rx = 0,
			tx = 0;
		for (const p of points) {
			req += p.requests;
			fails += p.server_errors;
			rx += p.rx_bytes;
			tx += p.tx_bytes;
		}
		return { req, fails, rx, tx };
	});

	const count = (n: number) => (n >= 100 ? Math.round(n).toLocaleString() : String(Math.round(n * 10) / 10));
	const cores = (c: number) => (c >= 1 ? c.toFixed(2).replace(/\.?0+$/, '') : c.toFixed(2));
	function bytes(n: number): string {
		if (n < 1024) return `${Math.round(n)} B`;
		if (n < 2 ** 20) return `${(n / 1024).toFixed(n < 10240 ? 1 : 0)} KB`;
		return megabytes(n / 2 ** 20);
	}
	const percent = (x: number) => `${x < 0.01 && x > 0 ? (x * 100).toFixed(2) : (x * 100).toFixed(1)}%`;
</script>

<div class="flex flex-col gap-6">
	<div class="flex flex-wrap items-end justify-between gap-4">
		<div>
			<h2 class="font-medium">{t('metrics.title')}</h2>
			<p class="text-sm text-muted">{t('metrics.lead')}</p>
		</div>
		<div class="flex rounded-full bg-selected p-0.5 text-sm" role="group" aria-label={t('metrics.range')}>
			{#each ['1h', '24h'] as const as r (r)}
				<button
					class="rounded-full px-3 py-1 transition {range === r ? 'bg-bg text-fg shadow-sm' : 'text-muted hover:text-fg'}"
					aria-pressed={range === r}
					onclick={() => (range = r)}>{t(r === '1h' ? 'metrics.hour' : 'metrics.day')}</button
				>
			{/each}
		</div>
	</div>

	{#if data && points.length === 0}
		<div class="flex flex-col items-start gap-3 rounded-2xl border border-dashed border-line p-5">
			<span class="grid size-10 place-items-center rounded-xl bg-selected"><Activity size={20} strokeWidth={1.75} /></span>
			<p class="text-[15px]">{t('metrics.empty')}</p>
			<p class="text-sm text-muted">{t('metrics.emptyHint')}</p>
		</div>
	{:else if data}
		<dl class="grid grid-cols-2 gap-px overflow-hidden rounded-2xl border border-line bg-line sm:grid-cols-4">
			{#if app.domain}
				<div class="bg-bg px-4 py-3">
					<dt class="text-sm text-muted">{t('metrics.requests')}</dt>
					<dd class="text-lg">{count(totals.req)}</dd>
				</div>
				<div class="bg-bg px-4 py-3">
					<dt class="text-sm text-muted">{t('metrics.errors')}</dt>
					<dd class="text-lg {totals.fails ? 'text-danger' : ''}">{totals.req ? percent(totals.fails / totals.req) : '–'}</dd>
				</div>
			{/if}
			<div class="bg-bg px-4 py-3">
				<dt class="text-sm text-muted">{t('metrics.traffic')}</dt>
				<dd class="text-lg">{bytes(totals.rx + totals.tx)}</dd>
			</div>
			<div class="bg-bg px-4 py-3">
				<dt class="text-sm text-muted">{t('metrics.crashes')}</dt>
				<dd class="text-lg {data.crashes ? 'text-warn' : ''}">{data.crashes}</dd>
			</div>
		</dl>

		<div class="grid gap-x-8 gap-y-8 lg:grid-cols-2">
			{#if app.domain}
				<section class="flex flex-col gap-3">
					<h3 class="text-sm font-medium">{t('metrics.requestsChart')}</h3>
					<Chart
						{times}
						step={data.step}
						format={count}
						series={[
							{ label: t('metrics.all'), color: 'var(--fg)', values: points.map((p) => p.requests / perMin) },
							{ label: t('metrics.serverErrors'), color: 'var(--danger)', values: points.map((p) => p.server_errors / perMin) }
						]}
					/>
				</section>
			{/if}
			<section class="flex flex-col gap-3">
				<h3 class="text-sm font-medium">{t('metrics.cpuChart')}</h3>
				<Chart {times} step={data.step} format={cores} max={app.cpus} series={[{ label: 'CPU', color: 'var(--fg)', values: points.map((p) => p.cpu) }]} />
			</section>
			<section class="flex flex-col gap-3">
				<h3 class="text-sm font-medium">{t('metrics.memoryChart')}</h3>
				<Chart
					{times}
					step={data.step}
					format={bytes}
					max={app.memory_mb * 2 ** 20}
					series={[{ label: t('metrics.memory'), color: 'var(--fg)', values: points.map((p) => p.memory_bytes) }]}
				/>
			</section>
			<section class="flex flex-col gap-3">
				<h3 class="text-sm font-medium">{t('metrics.networkChart')}</h3>
				<Chart
					{times}
					step={data.step}
					format={(v) => bytes(v) + '/s'}
					series={[
						{ label: t('metrics.in'), color: 'var(--fg)', values: points.map((p) => p.rx_bytes / data!.step) },
						{ label: t('metrics.out'), color: 'var(--muted)', values: points.map((p) => p.tx_bytes / data!.step), dashed: true }
					]}
				/>
			</section>
		</div>
	{/if}
</div>
