<script lang="ts">
	import { apps } from '$lib/apps.svelte';
	import { host, loadHost, megabytes, type Usage } from '$lib/host.svelte';
	import { t } from '$lib/i18n';

	// Picks an app's memory and CPU limits against what the server has and
	// what the other apps were given. usage is the app's own, when running.
	let {
		memory = $bindable(512),
		cpus = $bindable(1),
		app = '',
		usage = null
	}: { memory?: number; cpus?: number; app?: string; usage?: Usage | null } = $props();

	$effect(() => {
		loadHost();
	});

	const hostMB = $derived(host.info ? host.info.memory_bytes / 2 ** 20 : 8192);
	const hostCPUs = $derived(host.info?.cpus ?? 8);
	// What the other apps that run may use at most. Limits can add up to
	// more than the server has; the bar shows when they do.
	const others = $derived(
		apps.list
			.filter((a) => a.id !== app && !a.stopped && a.state !== 'none')
			.reduce((sum, a) => ({ memory: sum.memory + a.memory_mb, cpus: sum.cpus + a.cpus }), { memory: 0, cpus: 0 })
	);

	const presets = $derived(
		[
			{ name: t('resources.small'), memory: 256, cpus: 0.5 },
			{ name: t('resources.medium'), memory: 512, cpus: 1 },
			{ name: t('resources.large'), memory: 1024, cpus: 2 },
			{ name: t('resources.xl'), memory: 2048, cpus: 4 }
		].filter((p) => p.memory <= hostMB && p.cpus <= hostCPUs)
	);

	// Memory moves in steps that grow with the size, so the slider is as
	// useful on a small VPS as on a large server.
	const memorySteps = $derived.by(() => {
		const steps = [64, 128, 192, 256, 384, 512, 768];
		for (let mb = 1024; mb <= hostMB; mb += mb < 4096 ? 512 : mb < 16384 ? 2048 : 8192) steps.push(mb);
		const fit = steps.filter((s) => s <= hostMB);
		if (!fit.includes(memory)) fit.push(memory);
		return fit.sort((a, b) => a - b);
	});
	const cpuSteps = $derived.by(() => {
		const steps: number[] = [];
		for (let c = 0.25; c <= hostCPUs; c += 0.25) steps.push(c);
		if (!steps.includes(cpus)) steps.push(cpus);
		return steps.sort((a, b) => a - b);
	});

	const pct = (part: number, whole: number) => `${Math.max(0, Math.min(100, (part / whole) * 100))}%`;
	const cpuText = (c: number) => `${Number.isInteger(c) ? c : c.toFixed(2).replace(/0$/, '')} CPU`;
</script>

{#snippet meter(label: string, value: string, whole: string, mine: number, otherShare: number, total: number, used: number | null, usedText: string, otherText: string)}
	<div class="flex flex-col gap-2">
		<div class="flex items-baseline justify-between gap-4">
			<span class="text-sm font-medium">{label}</span>
			<span class="text-[15px]">{value} <span class="text-muted">{t('resources.of', { total: whole })}</span></span>
		</div>
		<div class="relative h-2 overflow-hidden rounded-full bg-selected" aria-hidden="true">
			<div class="absolute inset-y-0 left-0 bg-fg" style:width={pct(mine, total)}></div>
			<div class="absolute inset-y-0 bg-muted/35" style:left={pct(mine, total)} style:width={pct(otherShare, total)}></div>
			{#if used !== null}
				<div class="absolute inset-y-0 left-0 bg-ok" style:width={pct(Math.min(used, mine), total)}></div>
			{/if}
		</div>
		<p class="flex flex-wrap gap-x-3 text-sm text-muted">
			{#if used !== null}<span class="inline-flex items-center gap-1.5"><span class="size-2 rounded-full bg-ok"></span>{usedText}</span>{/if}
			{#if otherShare > 0}<span class="inline-flex items-center gap-1.5"><span class="size-2 rounded-full bg-muted/35"></span>{otherText}</span>{/if}
		</p>
	</div>
{/snippet}

<div class="flex flex-col gap-6">
	{#if presets.length}
		<div class="flex flex-wrap gap-2" role="group" aria-label={t('resources.presets')}>
			{#each presets as p (p.name)}
				{@const on = p.memory === memory && p.cpus === cpus}
				<button
					type="button"
					class="rounded-full border px-3.5 py-1.5 text-sm transition {on ? 'border-accent bg-accent text-accent-fg' : 'border-line hover:bg-hover'}"
					aria-pressed={on}
					onclick={() => ((memory = p.memory), (cpus = p.cpus))}
				>
					{p.name} <span class={on ? 'opacity-70' : 'text-muted'}>{megabytes(p.memory)} · {cpuText(p.cpus)}</span>
				</button>
			{/each}
		</div>
	{/if}

	<div class="flex flex-col gap-2">
		{@render meter(
			t('resources.memory'),
			megabytes(memory),
			megabytes(hostMB),
			memory,
			others.memory,
			hostMB,
			usage?.running && usage.memory_bytes !== undefined ? usage.memory_bytes / 2 ** 20 : null,
			t('resources.inUse', { amount: megabytes((usage?.memory_bytes ?? 0) / 2 ** 20) }),
			t('resources.others', { amount: megabytes(others.memory) })
		)}
		<input
			type="range"
			class="w-full accent-[var(--fg)]"
			aria-label={t('resources.memory')}
			min="0"
			max={memorySteps.length - 1}
			value={memorySteps.indexOf(memory)}
			oninput={(e) => (memory = memorySteps[Number(e.currentTarget.value)])}
		/>
	</div>

	<div class="flex flex-col gap-2">
		{@render meter(
			t('resources.cpu'),
			cpuText(cpus),
			cpuText(hostCPUs),
			cpus,
			others.cpus,
			hostCPUs,
			usage?.running && usage.cpu !== undefined ? usage.cpu : null,
			(usage?.cpu ?? 0) < 0.01 ? t('resources.cpuIdle') : t('resources.cpuInUse', { amount: cpuText(Math.round((usage?.cpu ?? 0) * 100) / 100) }),
			t('resources.others', { amount: cpuText(others.cpus) })
		)}
		<input
			type="range"
			class="w-full accent-[var(--fg)]"
			aria-label={t('resources.cpu')}
			min="0"
			max={cpuSteps.length - 1}
			value={cpuSteps.indexOf(cpus)}
			oninput={(e) => (cpus = cpuSteps[Number(e.currentTarget.value)])}
		/>
	</div>

	{#if host.info}
		<p class="text-sm text-muted">
			{t('resources.disk', {
				free: megabytes(host.info.disk_free_bytes / 2 ** 20),
				total: megabytes(host.info.disk_bytes / 2 ** 20)
			})}
		</p>
	{/if}
	{#if others.memory + memory > hostMB}
		<p class="text-sm text-danger">{t('resources.overMemory')}</p>
	{/if}
</div>
