<script lang="ts">
	import { Check, Download, Gamepad2, Search } from '@lucide/svelte';
	import { goto } from '$app/navigation';
	import { api, ApiError } from '$lib/api';
	import { apps, reload } from '$lib/apps.svelte';
	import { messageOf } from '$lib/errors';
	import { eulaLink, type Allocation, type EggPreview } from '$lib/games.svelte';
	import { host, loadHost, megabytes } from '$lib/host.svelte';
	import { say, t } from '$lib/i18n';
	import { freePorts, portLabel } from '$lib/ports';
	import { defaultSize, sizeSteps } from '$lib/volumes';
	import Button from '$lib/ui/Button.svelte';
	import ErrorText from '$lib/ui/ErrorText.svelte';
	import Field from '$lib/ui/Field.svelte';
	import Lead from '$lib/ui/Lead.svelte';
	import PoolAdd from '$lib/ui/PoolAdd.svelte';
	import Resources from '$lib/ui/Resources.svelte';

	// memory_mb, disk_mb and ports are what the game needs, for games that need more than most.
	type Entry = { id: string; name: string; game: string; memory_mb?: number; disk_mb?: number; ports?: number };
	const defaultMemory = 4096;
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
	let memory = $state(defaultMemory);
	let cpus = $state(2);
	let disk = $state(10240);
	// Ports beyond the named ones, for a game that needs more than its egg
	// says, such as a plugin's.
	let extra = $state(0);
	let eula = $state(false);
	let values = $state<Record<string, string>>({});
	// Server complaints about one variable, by its environment name.
	let problems = $state<Record<string, string>>({});

	let pool = $state<Allocation[] | null>(null);
	let addOpen = $state(false);
	let poolError = $state('');

	const freeList = $derived(freePorts(pool ?? []));
	const free = $derived(freeList.length);

	// The ports of the server by role: the one players join on, then one for
	// each port variable the egg has. What the server would pick by itself
	// is filled in; a choice replaces it.
	type Role = { key: string; label: string; env: string };
	const roles = $derived.by(() => {
		const list: Role[] = [{ key: '', label: t('game.new.rolePrimary'), env: '' }];
		for (const v of preview?.port_variables ?? []) list.push({ key: v.env, label: v.name, env: v.env });
		return list;
	});
	const ports = $derived(roles.length + extra);
	// Allocation ids picked by hand, by role key.
	let picks = $state<Record<string, number>>({});
	const taken = $derived(new Set(roles.flatMap((r) => (r.key in picks ? [picks[r.key]] : []))));
	const shown = $derived.by(() => {
		const spare = freeList.filter((a) => !taken.has(a.id));
		let next = 0;
		const out: Record<string, number | undefined> = {};
		for (const r of roles) out[r.key] = picks[r.key] ?? spare[next++]?.id;
		return out;
	});
	const optionsFor = (r: Role) => freeList.filter((a) => a.id === shown[r.key] || !Object.values(shown).includes(a.id));

	const diskMB = $derived(host.info ? host.info.disk_bytes / 2 ** 20 : 102400);
	// Variables with a port are set by the ports step; the ones the egg
	// locks are only offered to administrators, out of the way.
	const shownVars = $derived(preview?.variables.filter((v) => v.editable && !v.port) ?? []);
	const editable = $derived(shownVars.filter((v) => !v.locked));
	const lockedVars = $derived(shownVars.filter((v) => v.locked));
	const needsEula = $derived(preview?.features.includes('eula') ?? false);
	const entry = $derived(catalog.find((c) => c.id === chosen));
	const cardName = $derived(entry ? (entry.name === entry.game ? entry.name : `${entry.game} ${entry.name}`) : '');
	// One card per game; a game with several eggs, such as Minecraft, offers
	// its kinds under the cards once it is picked.
	type Group = { game: string; entries: Entry[] };
	const groups = $derived.by(() => {
		const list: Group[] = [];
		for (const c of catalog) {
			const g = list.find((x) => x.game === c.game);
			if (g) g.entries.push(c);
			else list.push({ game: c.game, entries: [c] });
		}
		return list;
	});
	let query = $state('');
	const shownGroups = $derived.by(() => {
		const q = query.trim().toLowerCase();
		if (!q) return groups;
		return groups.filter((g) => g.game.toLowerCase().includes(q) || g.entries.some((e) => e.name.toLowerCase().includes(q)));
	});
	function pickGroup(g: Group) {
		if (!g.entries.some((e) => e.id === chosen)) chosen = g.entries[0].id;
	}
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
			picks = {};
			eula = false;
			if (!named) name = suggestName(preview.name);
			extra = 0;
			if (host.info) {
				memory = Math.min(entry?.memory_mb ?? defaultMemory, Math.floor(host.info.memory_bytes / 2 ** 20));
				cpus = Math.min(Math.max(cpus, 1), host.info.cpus);
				const fit = sizeSteps(diskMB).filter((s) => s <= diskMB / 2);
				disk = entry?.disk_mb ? Math.floor(Math.min(entry.disk_mb, diskMB / 2)) : fit.includes(disk) ? disk : (fit[fit.length - 1] ?? defaultSize);
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
		}
	}

	async function added() {
		addOpen = false;
		await loadPool();
	}

	function toPorts(e: SubmitEvent) {
		e.preventDefault();
		step = 3;
		if (pool === null) loadPool();
	}

	function toSettings() {
		error = '';
		if (shownVars.length || needsEula) step = 4;
		else create();
	}

	// The step whose fields a server error is about.
	function stepFor(code: string): number {
		if (/^game\.(bad_variable|variable_locked|unknown_variable|eula_required)$/.test(code)) return 4;
		if (/^(game\.(bad_memory|bad_cpus|bad_image)|app\.(bad_name|exists))$/.test(code)) return 2;
		if (/^(allocation\.|game\.(bad_ports|no_port_role))/.test(code)) return 3;
		return step;
	}

	async function create() {
		problems = {};
		error = '';
		for (const v of shownVars) {
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
				// What is left as the egg has it needs no saying, and a locked
				// default is not the customer's to have chosen.
				variables: Object.fromEntries(shownVars.filter((v) => values[v.env] !== v.value).map((v) => [v.env, values[v.env]]))
			};
			const byHand = roles.filter((r) => r.key in picks);
			if (byHand.length) {
				body.allocations = {
					primary: picks[''] ?? 0,
					variables: Object.fromEntries(byHand.filter((r) => r.env).map((r) => [r.env, picks[r.key]]))
				};
			}
			if (needsEula) body.accept_eula = eula;
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
			{#if groups.length > 8}
				<div class="relative">
					<Search size={15} class="pointer-events-none absolute top-1/2 left-3.5 -translate-y-1/2 text-muted" />
					<input
						class="h-10 w-full min-w-0 rounded-xl border border-line bg-bg pr-3.5 pl-9 text-[15px] outline-none transition focus:border-muted"
						type="search"
						bind:value={query}
						placeholder={t('game.new.search')}
						aria-label={t('game.new.search')}
						autocomplete="off"
					/>
				</div>
			{/if}
			<div class="grid grid-flow-dense gap-3 sm:grid-cols-2">
				{#each shownGroups as g (g.game)}
					{@const on = g.entries.some((e) => e.id === chosen)}
					<button
						type="button"
						class="flex items-start gap-4 rounded-2xl border p-4 text-left transition hover:bg-hover {on ? 'border-fg' : 'border-line'}"
						aria-pressed={on}
						onclick={() => pickGroup(g)}
					>
						<span class="inline-flex size-10 shrink-0 items-center justify-center rounded-xl border border-line bg-panel" aria-hidden="true"><Gamepad2 size={20} strokeWidth={1.5} /></span>
						<span class="min-w-0">
							<span class="block font-medium">{g.game}</span>
							{#if g.entries.length > 1}
								<span class="block text-sm text-muted">{t('game.new.kinds', { n: g.entries.length })}</span>
							{:else if g.entries[0].name !== g.game}
								<span class="block text-sm text-muted">{g.entries[0].name}</span>
							{/if}
						</span>
					</button>
					{#if on && g.entries.length > 1}
						<div class="flex flex-col gap-2 sm:col-span-2" role="group" aria-label={t('game.new.kind', { game: g.game })}>
							<p class="text-sm font-medium">{t('game.new.kind', { game: g.game })}</p>
							<div class="flex flex-wrap gap-2">
								{#each g.entries as k (k.id)}
									<button
										type="button"
										class="h-9 rounded-full border px-4 text-sm transition hover:bg-hover {chosen === k.id ? 'border-fg' : 'border-line'}"
										aria-pressed={chosen === k.id}
										onclick={() => (chosen = k.id)}>{k.name}</button
									>
								{/each}
							</div>
						</div>
					{/if}
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
			{#if query.trim() && !shownGroups.length}
				<p class="text-sm text-muted">{t('game.new.noMatch', { query: query.trim() })}</p>
			{/if}
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
				<PoolAdd onadded={added} oncancel={addOpen ? () => (addOpen = false) : undefined} />
			{:else}
				<p class="text-sm text-muted">
					{t('game.new.free', { n: free })}
					<button type="button" class="text-fg underline decoration-line underline-offset-2 hover:decoration-fg" onclick={() => (addOpen = true)}>{t('game.new.addMore')}</button>
				</p>
			{/if}
			<ErrorText message={poolError && pool !== null ? poolError : ''} />
			{#if free > 0}
				<ul class="flex flex-col divide-y divide-line rounded-2xl border border-line" aria-label={t('game.new.roles')}>
					{#each roles as r (r.key)}
						<li class="flex flex-wrap items-center justify-between gap-x-4 gap-y-2 px-4 py-3">
							<label for="port-{r.key || 'primary'}" class="min-w-0 text-[15px]">
								{r.label}{#if r.env}<span class="ml-1.5 font-mono text-xs text-muted">· {r.env}</span>{/if}
							</label>
							<select
								id="port-{r.key || 'primary'}"
								class="h-10 min-w-28 rounded-xl border border-line bg-bg px-3 font-mono text-[15px]"
								value={shown[r.key]}
								onchange={(e) => (picks[r.key] = Number(e.currentTarget.value))}
							>
								{#each optionsFor(r) as a (a.id)}<option value={a.id}>{portLabel(a)}</option>{/each}
							</select>
						</li>
					{/each}
				</ul>
				<div class="flex flex-wrap items-center gap-x-3 gap-y-1 text-sm text-muted">
					{#if extra > 0}<span>{t('game.new.extraPorts', { n: extra })}</span>{/if}
					{#if ports < Math.min(16, free)}
						<button type="button" class="text-fg underline decoration-line underline-offset-2 hover:decoration-fg" onclick={() => extra++}>{t('game.new.addPort')}</button>
					{/if}
					{#if extra > 0}
						<button type="button" class="text-fg underline decoration-line underline-offset-2 hover:decoration-fg" onclick={() => extra--}>{t('game.new.removePort')}</button>
					{/if}
				</div>
			{/if}
			<ErrorText message={error} />
			<div class="flex items-center gap-3">
				<Button disabled={free < ports || ports > 16 || free === 0} onclick={toSettings} busy={busy}>{shownVars.length || needsEula ? t('common.continue') : t('game.new.create')}</Button>
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
				{#if shownVars.length}<p class="text-sm text-muted">{t('game.new.settingsLead')}</p>{/if}
			</div>
			{#each editable as v (v.env)}
				<div class="flex flex-col gap-1.5">
					<Field label={v.name} hint={v.description} placeholder={v.default} autocomplete="off" bind:value={values[v.env]} oninput={() => delete problems[v.env]} />
					<ErrorText message={problems[v.env] ?? ''} />
				</div>
			{/each}
			{#if lockedVars.length}
				<details class="rounded-2xl border border-line px-4 py-3">
					<summary class="cursor-pointer text-[15px]">{t('game.new.lockedVars', { n: lockedVars.length })}</summary>
					<div class="mt-4 flex flex-col gap-5">
						{#each lockedVars as v (v.env)}
							<div class="flex flex-col gap-1.5">
								<Field label={v.name} hint={v.description} placeholder={v.default} autocomplete="off" bind:value={values[v.env]} oninput={() => delete problems[v.env]} />
								<p class="text-xs text-muted">{t('startup.lockedForUsers')}</p>
								<ErrorText message={problems[v.env] ?? ''} />
							</div>
						{/each}
					</div>
				</details>
			{/if}
			{#if needsEula}
				<div class="flex flex-col gap-1.5">
					<label class="flex items-start gap-2.5 text-[15px]">
						<input type="checkbox" class="mt-1" bind:checked={eula} />
						<span>{t('game.new.eula')}<span class="block text-sm text-muted">{t('game.new.eulaHint')}</span></span>
					</label>
					<a href={eulaLink} target="_blank" rel="noopener noreferrer" class="ml-6 w-fit text-sm underline decoration-line underline-offset-2 hover:decoration-fg">{t('game.new.eulaLink')}</a>
				</div>
			{/if}
			<ErrorText message={error} />
			<div class="flex items-center gap-3">
				<Button type="submit" {busy} disabled={needsEula && !eula}>{t('game.new.create')}</Button>
				<Button kind="quiet" type="button" onclick={() => ((error = ''), (step = 3))}>{t('new.back')}</Button>
			</div>
		</form>
	{/if}
</div>
