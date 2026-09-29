<script lang="ts">
	import { Check, Download, Gamepad2 } from '@lucide/svelte';
	import { goto } from '$app/navigation';
	import { api, ApiError } from '$lib/api';
	import { apps, reload } from '$lib/apps.svelte';
	import { messageOf } from '$lib/errors';
	import type { Allocation, EggPreview, PortRange } from '$lib/games.svelte';
	import { host, loadHost, megabytes } from '$lib/host.svelte';
	import { say, t } from '$lib/i18n';
	import { defaultSize, sizeSteps } from '$lib/volumes';
	import Button from '$lib/ui/Button.svelte';
	import ErrorText from '$lib/ui/ErrorText.svelte';
	import Field from '$lib/ui/Field.svelte';
	import Lead from '$lib/ui/Lead.svelte';
	import Resources from '$lib/ui/Resources.svelte';

	type Entry = { id: string; name: string; game: string };
	const steps = $derived([t('game.new.step.game'), t('game.new.step.resources'), t('game.new.step.ports'), t('game.new.step.settings')]);

	let step = $state(1);
	let catalog = $state<Entry[]>([]);
	// A catalog id, or 'url' for an egg from an address.
	let chosen = $state('');
	let eggUrl = $state('');
	let preview = $state<EggPreview | null>(null);
	let loading = $state(false);
	let error = $state('');
	let busy = $state(false);

	let name = $state('');
	let named = $state(false);
	let image = $state('');
	let memory = $state(4096);
	let cpus = $state(2);
	let disk = $state(10240);
	let portsText = $state('1');
	const ports = $derived(Math.max(1, Math.round(Number(portsText)) || 1));
	let values = $state<Record<string, string>>({});
	// Server complaints about one variable, by its environment name.
	let problems = $state<Record<string, string>>({});

	let pool = $state<Allocation[] | null>(null);
	let range = $state('');
	let suggestion = $state('');
	let adding = $state(false);
	let addOpen = $state(false);
	let poolError = $state('');

	const free = $derived((pool ?? []).filter((a) => !a.app).length);
	const diskMB = $derived(host.info ? host.info.disk_bytes / 2 ** 20 : 102400);
	const editable = $derived(preview?.variables.filter((v) => v.editable) ?? []);
	const cardName = $derived(catalog.find((c) => c.id === chosen)?.name ?? '');
	const urlValid = $derived(/^https:\/\/\S+$/.test(eggUrl.trim()));

	$effect(() => {
		loadHost();
		api<Entry[]>('GET', '/eggs/catalog')
			.then((list) => (catalog = list))
			.catch((err) => (error = messageOf(err)));
	});

	const slug = (s: string) =>
		s
			.toLowerCase()
			.replace(/[^a-z0-9-]+/g, '-')
			.replace(/^-+|-+$/g, '')
			.slice(0, 36);

	function suggestName(base: string): string {
		const taken = new Set(apps.list.map((a) => a.id));
		const root = slug(base) || 'game';
		let n = 1;
		while (taken.has(n === 1 ? root : `${root}-${n}`)) n++;
		return n === 1 ? root : `${root}-${n}`;
	}

	async function pick() {
		loading = true;
		error = '';
		try {
			const body = chosen === 'url' ? { egg_url: eggUrl.trim() } : { egg: chosen };
			preview = await api<EggPreview>('POST', '/eggs/preview', body);
			image = preview.images[0]?.ref ?? '';
			values = Object.fromEntries(preview.variables.map((v) => [v.env, v.value]));
			problems = {};
			if (!named) name = suggestName(preview.name);
			if (host.info) {
				memory = Math.min(memory, Math.floor(host.info.memory_bytes / 2 ** 20));
				cpus = Math.min(Math.max(cpus, 1), host.info.cpus);
				const fit = sizeSteps(diskMB).filter((s) => s <= diskMB / 2);
				disk = fit.includes(disk) ? disk : (fit[fit.length - 1] ?? defaultSize);
			}
			step = 2;
		} catch (err) {
			error = messageOf(err);
		} finally {
			loading = false;
		}
	}

	async function loadPool() {
		poolError = '';
		try {
			pool = await api<Allocation[]>('GET', '/nodes/1/allocations');
		} catch (err) {
			poolError = messageOf(err);
			return;
		}
		if (!free) await suggest();
	}

	async function suggest() {
		try {
			const r = await api<PortRange>('GET', '/nodes/1/allocations/suggest');
			suggestion = r.ports;
			range = r.ports;
			poolError = '';
		} catch (err) {
			suggestion = '';
			poolError = messageOf(err);
		}
	}

	async function addToPool(e: SubmitEvent) {
		e.preventDefault();
		adding = true;
		poolError = '';
		try {
			await api('POST', '/nodes/1/allocations', { ports: range.trim() });
			addOpen = false;
			await loadPool();
		} catch (err) {
			poolError = messageOf(err);
		} finally {
			adding = false;
		}
	}

	function toPorts(e: SubmitEvent) {
		e.preventDefault();
		step = 3;
		if (pool === null) loadPool();
	}

	function toSettings() {
		error = '';
		if (editable.length) step = 4;
		else create();
	}

	// The step whose fields a server error is about.
	function stepFor(code: string): number {
		if (/^game\.(bad_variable|variable_locked|unknown_variable)$/.test(code)) return 4;
		if (/^(game\.(bad_memory|bad_cpus|bad_image)|app\.(bad_name|exists))$/.test(code)) return 2;
		if (/^(allocation\.|game\.bad_ports)/.test(code)) return 3;
		return step;
	}

	async function create() {
		problems = {};
		error = '';
		for (const v of editable) {
			if (!values[v.env].trim() && v.rules?.includes('required')) problems[v.env] = t('game.new.required', { name: v.name });
		}
		if (Object.keys(problems).length) {
			step = 4;
			return;
		}
		busy = true;
		try {
			const body: Record<string, unknown> = {
				name: name.trim(),
				image,
				memory_mb: memory,
				cpus,
				disk_mb: disk,
				ports,
				variables: Object.fromEntries(editable.map((v) => [v.env, values[v.env]]))
			};
			if (chosen === 'url') body.egg_url = eggUrl.trim();
			else body.egg = chosen;
			await api('POST', '/games', body);
			await reload();
			await goto(`/g/${name.trim()}`);
		} catch (err) {
			if (err instanceof ApiError) {
				step = stepFor(err.msg.code);
				const env = err.msg.params?.name;
				if (err.msg.code === 'game.bad_variable' && typeof env === 'string' && env in values) {
					problems[env] = say(err.msg);
					return;
				}
			}
			error = messageOf(err);
		} finally {
			busy = false;
		}
	}
</script>

<div class="flex max-w-2xl flex-col gap-8">
	<div class="flex items-center gap-4">
		<span class="inline-flex size-12 shrink-0 items-center justify-center rounded-2xl border border-line bg-panel" aria-hidden="true"><Gamepad2 size={24} strokeWidth={1.5} /></span>
		<Lead title={t('game.new.title')} text={' ' + t('game.new.lead')} />
	</div>

	<ol class="flex flex-wrap gap-x-5 gap-y-2 text-sm" aria-label={t('game.new.steps')}>
		{#each steps as label, i (i)}
			<li class="flex items-center gap-2 {step === i + 1 ? 'text-fg' : 'text-muted'}" aria-current={step === i + 1 ? 'step' : undefined}>
				<span class="grid size-6 place-items-center rounded-full text-xs font-medium {step === i + 1 ? 'bg-accent text-accent-fg' : 'bg-selected'}"
					>{#if step > i + 1}<Check size={13} />{:else}{i + 1}{/if}</span
				>{label}
			</li>
		{/each}
	</ol>

	{#if step === 1}
		<section class="flex flex-col gap-4">
			<div>
				<h2 class="font-medium">{t('game.new.gameTitle')}</h2>
				<p class="text-sm text-muted">{t('game.new.gameLead')}</p>
			</div>
			<div class="grid gap-3 sm:grid-cols-2">
				{#each catalog as c (c.id)}
					<button
						type="button"
						class="flex items-start gap-4 rounded-2xl border p-4 text-left transition hover:bg-hover {chosen === c.id ? 'border-fg' : 'border-line'}"
						aria-pressed={chosen === c.id}
						onclick={() => (chosen = c.id)}
					>
						<span class="inline-flex size-10 shrink-0 items-center justify-center rounded-xl border border-line bg-panel" aria-hidden="true"><Gamepad2 size={20} strokeWidth={1.5} /></span>
						<span class="min-w-0">
							<span class="block font-medium">{c.game}</span>
							{#if c.name !== c.game}<span class="block text-sm text-muted">{c.name}</span>{/if}
						</span>
					</button>
				{/each}
				<button
					type="button"
					class="flex items-start gap-4 rounded-2xl border p-4 text-left transition hover:bg-hover {chosen === 'url' ? 'border-fg' : 'border-line'}"
					aria-pressed={chosen === 'url'}
					onclick={() => (chosen = 'url')}
				>
					<span class="inline-flex size-10 shrink-0 items-center justify-center rounded-xl border border-line bg-panel" aria-hidden="true"><Download size={20} strokeWidth={1.5} /></span>
					<span class="min-w-0">
						<span class="block font-medium">{t('game.new.import')}</span>
						<span class="block text-sm text-muted">{t('game.new.importLead')}</span>
					</span>
				</button>
			</div>
			{#if chosen === 'url'}
				<Field label={t('game.new.eggUrl')} hint={t('game.new.eggUrlHint')} type="url" placeholder="https://…/egg-my-game.json" autocomplete="off" bind:value={eggUrl} />
			{/if}
			<ErrorText message={error} />
			<div class="flex items-center gap-3">
				<Button busy={loading} disabled={!chosen || (chosen === 'url' && !urlValid)} onclick={pick}>{t('common.continue')}</Button>
				<Button kind="quiet" type="button" onclick={() => goto('/new')}>{t('new.back')}</Button>
			</div>
		</section>
	{:else if step === 2 && preview}
		<form class="flex flex-col gap-6" onsubmit={toPorts}>
			<div>
				<h2 class="font-medium">{t('game.new.resourcesTitle', { egg: preview.name })}</h2>
				<p class="text-sm text-muted">{preview.description || t('game.new.resourcesLead')}</p>
			</div>
			<Field
				label={t('new.name')}
				hint={t('game.new.nameHint')}
				pattern="[a-z0-9][a-z0-9\-]*"
				maxlength={40}
				required
				autocomplete="off"
				bind:value={name}
				oninput={() => (named = true)}
			/>
			<Resources bind:memory bind:cpus game />
			<div class="flex flex-col gap-1.5">
				<label for="disk" class="text-sm font-medium">{t('game.new.disk')}</label>
				<select id="disk" class="h-10 w-40 rounded-xl border border-line bg-bg px-3 text-[15px]" bind:value={disk}>
					{#each sizeSteps(diskMB / 2, disk) as mb (mb)}<option value={mb}>{megabytes(mb)}</option>{/each}
				</select>
				<p class="text-sm text-muted">{t('game.new.diskHint')}</p>
			</div>
			{#if preview.images.length > 1}
				<div class="flex flex-col gap-1.5">
					<label for="image" class="text-sm font-medium">{t('game.new.image')}</label>
					<select id="image" class="h-10 w-full rounded-xl border border-line bg-bg px-3 text-[15px]" bind:value={image}>
						{#each preview.images as img (img.ref)}<option value={img.ref}>{img.label}</option>{/each}
					</select>
					<p class="text-sm text-muted">{t('game.new.imageHint')}</p>
				</div>
			{/if}
			<ErrorText message={error} />
			<div class="flex items-center gap-3">
				<Button type="submit">{t('common.continue')}</Button>
				<Button kind="quiet" type="button" onclick={() => ((error = ''), (step = 1))}>{t('new.back')}</Button>
			</div>
		</form>
	{:else if step === 3}
		<section class="flex flex-col gap-5">
			<div>
				<h2 class="font-medium">{t('game.new.portsTitle')}</h2>
				<p class="text-sm text-muted">{t('game.new.portsLead')}</p>
			</div>
			{#if pool === null}
				<p class="text-sm text-muted">{poolError || t('game.new.portsLoading')}</p>
			{:else if free === 0 || addOpen}
				<form class="flex flex-col gap-3 rounded-2xl border border-line p-4" onsubmit={addToPool}>
					<p class="text-[15px]">
						<span class="font-medium">{suggestion ? t('game.new.found', { ports: suggestion.replace('-', '–') }) : t('game.new.foundNone')}</span>
						<span class="text-muted">{' ' + t('game.new.foundLead')}</span>
					</p>
					<Field label={t('game.new.poolPorts')} hint={t('game.new.poolPortsHint')} placeholder="25565-25664" required autocomplete="off" bind:value={range} />
					<div class="flex items-center gap-3">
						<Button type="submit" busy={adding} disabled={!range.trim()}>{t('game.new.addPool')}</Button>
						{#if addOpen}<Button kind="quiet" type="button" onclick={() => (addOpen = false)}>{t('common.cancel')}</Button>{/if}
					</div>
				</form>
			{:else}
				<p class="text-sm text-muted">
					{t('game.new.free', { n: free })}
					<button type="button" class="text-fg underline decoration-line underline-offset-2 hover:decoration-fg" onclick={() => ((addOpen = true), suggest())}>{t('game.new.addMore')}</button>
				</p>
			{/if}
			<ErrorText message={poolError && pool !== null ? poolError : ''} />
			{#if free > 0}
				<div class="max-w-48">
					<Field label={t('game.new.portCount')} hint={t('game.new.portCountHint')} type="number" min="1" max={Math.min(16, free)} bind:value={portsText} />
				</div>
			{/if}
			<ErrorText message={error} />
			<div class="flex items-center gap-3">
				<Button disabled={free < ports || ports > 16 || free === 0} onclick={toSettings} busy={busy}>{editable.length ? t('common.continue') : t('game.new.create')}</Button>
				<Button kind="quiet" type="button" onclick={() => ((error = ''), (step = 2))}>{t('new.back')}</Button>
			</div>
		</section>
	{:else if step === 4 && preview}
		<form
			class="flex flex-col gap-6"
			onsubmit={(e) => {
				e.preventDefault();
				create();
			}}
		>
			<div>
				<h2 class="font-medium">{t('game.new.settingsTitle', { egg: cardName || preview.name })}</h2>
				<p class="text-sm text-muted">{t('game.new.settingsLead')}</p>
			</div>
			{#each editable as v (v.env)}
				<div class="flex flex-col gap-1.5">
					<Field label={v.name} hint={v.description} placeholder={v.default} autocomplete="off" bind:value={values[v.env]} oninput={() => delete problems[v.env]} />
					<ErrorText message={problems[v.env] ?? ''} />
				</div>
			{/each}
			<ErrorText message={error} />
			<div class="flex items-center gap-3">
				<Button type="submit" {busy}>{t('game.new.create')}</Button>
				<Button kind="quiet" type="button" onclick={() => ((error = ''), (step = 3))}>{t('new.back')}</Button>
			</div>
		</form>
	{/if}
</div>
