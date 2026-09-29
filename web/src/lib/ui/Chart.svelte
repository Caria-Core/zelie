<script lang="ts">
	// A small line chart, drawn by hand so the panel stays light. Each
	// series is a line; the first is filled underneath. A point further
	// than two steps from the one before starts a new line, so an app that
	// was down shows a gap rather than a slope.
	export type Series = { label: string; color: string; values: number[]; dashed?: boolean };

	let {
		times,
		series,
		step,
		format,
		max = 0,
		height = 120
	}: {
		times: number[]; // ms since the epoch, oldest first
		series: Series[];
		step: number; // seconds each point covers
		format: (v: number) => string;
		max?: number; // the top of the scale, such as a limit; 0 fits the data
		height?: number;
	} = $props();

	let width = $state(0);
	let hover = $state<number | null>(null);

	const top = $derived(Math.max(max, ...series.flatMap((s) => s.values), 0) * (max ? 1 : 1.15) || 1);
	const start = $derived(times[0] ?? 0);
	const end = $derived(times.at(-1) ?? 1);
	const x = (t: number) => (end === start ? width / 2 : ((t - start) / (end - start)) * width);
	const y = (v: number) => height - (v / top) * height;

	// Runs of points with no gap between them.
	const runs = $derived.by(() => {
		const out: number[][] = [];
		times.forEach((t, i) => {
			if (i === 0 || t - times[i - 1] > 2 * step * 1000) out.push([]);
			out.at(-1)!.push(i);
		});
		return out;
	});

	function line(values: number[], run: number[]): string {
		return run.map((i, k) => `${k ? 'L' : 'M'}${x(times[i]).toFixed(1)},${y(values[i]).toFixed(1)}`).join('');
	}
	function area(values: number[], run: number[]): string {
		const first = x(times[run[0]]).toFixed(1);
		const last = x(times[run.at(-1)!]).toFixed(1);
		return `${line(values, run)}L${last},${height}L${first},${height}Z`;
	}

	const clock = (t: number) => new Date(t).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });

	function move(e: PointerEvent) {
		const r = (e.currentTarget as SVGElement).getBoundingClientRect();
		const at = start + ((e.clientX - r.left) / r.width) * (end - start);
		let best = 0;
		times.forEach((t, i) => {
			if (Math.abs(t - at) < Math.abs(times[best] - at)) best = i;
		});
		hover = times.length ? best : null;
	}
</script>

<div class="flex flex-col gap-1.5">
	<div class="relative" bind:clientWidth={width} style:height="{height}px">
		{#if width > 0 && times.length > 0}
			<svg
				{width}
				{height}
				class="block touch-none overflow-visible"
				role="img"
				aria-label={series.map((s) => `${s.label} ${format(s.values.at(-1) ?? 0)}`).join(', ')}
				onpointermove={move}
				onpointerleave={() => (hover = null)}
			>
				<line x1="0" x2={width} y1={height - 0.5} y2={height - 0.5} stroke="var(--line)" />
				<line x1="0" x2={width} y1="0.5" y2="0.5" stroke="var(--line)" stroke-dasharray="2 4" />
				{#each series as s, n (s.label)}
					{#each runs as run, k (k)}
						{#if n === 0 && run.length > 1}
							<path d={area(s.values, run)} fill={s.color} opacity="0.08" />
						{/if}
						{#if run.length === 1}
							<circle cx={x(times[run[0]])} cy={y(s.values[run[0]])} r="1.5" fill={s.color} />
						{:else}
							<path
								d={line(s.values, run)}
								fill="none"
								stroke={s.color}
								stroke-width="1.5"
								stroke-linejoin="round"
								stroke-dasharray={s.dashed ? '4 3' : undefined}
							/>
						{/if}
					{/each}
				{/each}
				{#if hover !== null}
					<line x1={x(times[hover])} x2={x(times[hover])} y1="0" y2={height} stroke="var(--muted)" stroke-dasharray="2 3" />
					{#each series as s (s.label)}
						<circle cx={x(times[hover])} cy={y(s.values[hover])} r="3" fill="var(--bg)" stroke={s.color} stroke-width="1.5" />
					{/each}
				{/if}
			</svg>
			<span class="pointer-events-none absolute top-0 right-0 text-xs text-muted">{format(top)}</span>
			{#if hover !== null}
				{@const left = x(times[hover]) > width / 2}
				<div
					class="pointer-events-none absolute top-4 z-10 rounded-lg border border-line bg-bg px-2.5 py-1.5 text-xs whitespace-nowrap shadow-sm"
					style:left={left ? undefined : `${x(times[hover]) + 8}px`}
					style:right={left ? `${width - x(times[hover]) + 8}px` : undefined}
				>
					<p class="text-muted">{clock(times[hover])}</p>
					{#each series as s (s.label)}
						<p><span style:color={s.color}>●</span> {s.label} {format(s.values[hover])}</p>
					{/each}
				</div>
			{/if}
		{/if}
	</div>
	{#if times.length > 0}
		<div class="flex justify-between gap-3 text-xs text-muted">
			<span>{clock(start)}</span>
			{#if series.length > 1}
				<span class="flex gap-3">
					{#each series as s (s.label)}
						<span class="inline-flex items-center gap-1.5"
							><svg width="14" height="2" aria-hidden="true"
								><line x1="0" x2="14" y1="1" y2="1" stroke={s.color} stroke-width="2" stroke-dasharray={s.dashed ? '4 3' : undefined} /></svg
							>{s.label}</span
						>
					{/each}
				</span>
			{/if}
			<span>{clock(end)}</span>
		</div>
	{/if}
</div>
