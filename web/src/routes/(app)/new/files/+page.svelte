<script lang="ts">
	import { Check, FolderCode } from '@lucide/svelte';
	import { goto } from '$app/navigation';
	import { api, ApiError } from '$lib/api';
	import { apps, reload, runtimeLabel } from '$lib/apps.svelte';
	import { messageOf } from '$lib/errors';
	import type { EggPreview } from '$lib/games.svelte';
	import { host, loadHost, megabytes } from '$lib/host.svelte';
	import { say, t } from '$lib/i18n';
	import { server } from '$lib/server.svelte';
	import { defaultSize, sizeSteps } from '$lib/volumes';
	import AppIcon from '$lib/ui/AppIcon.svelte';
	import Button from '$lib/ui/Button.svelte';
	import ErrorText from '$lib/ui/ErrorText.svelte';
	import Field from '$lib/ui/Field.svelte';
	import Lead from '$lib/ui/Lead.svelte';
	import Resources from '$lib/ui/Resources.svelte';

	type Entry = { id: string; name: string; game: string; memory_mb?: number };
	const steps = $derived([t('files.new.step.runtime'), t('files.new.step.app'), t('files.new.step.settings')]);

	let step = $state(1);
	let catalog = $state<Entry[]>([]);
	let chosen = $state('');
	let preview = $state<EggPreview | null>(null);
	let loading = $state(false);
	let error = $state('');
	let busy = $state(false);

	let name = $state('');
	let named = $state(false);
	let image = $state('');
	let domain = $state('');
	let memory = $state(512);
	let cpus = $state(1);
	let disk = $state(5120);
	let values = $state<Record<string, string>>({});
	// Server complaints about one variable, by its environment name.
	let problems = $state<Record<string, string>>({});

	const diskMB = $derived(host.info ? host.info.disk_bytes / 2 ** 20 : 102400);
	const editable = $derived(preview?.variables.filter((v) => v.editable) ?? []);
	const entry = $derived(catalog.find((c) => c.id === chosen));

	$effect(() => {
		loadHost();
		api<Entry[]>('GET', '/eggs/catalog?kind=runtime')
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
		const root = slug(base) || 'app';
		let n = 1;
		while (taken.has(n === 1 ? root : `${root}-${n}`)) n++;
		return n === 1 ? root : `${root}-${n}`;
	}

	async function pick() {
		loading = true;
		error = '';
		try {
			preview = await api<EggPreview>('POST', '/eggs/preview', { egg: chosen });
			image = preview.images[0]?.ref ?? '';
			values = Object.fromEntries(preview.variables.map((v) => [v.env, v.value]));
			problems = {};
			if (!named) name = suggestName(`${runtimeLabel[chosen] ?? chosen}-app`);
			if (host.info) {
				memory = Math.min(Math.max(entry?.memory_mb ?? 512, 512), Math.floor(host.info.memory_bytes / 2 ** 20));
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

	// The step whose fields a server error is about.
	function stepFor(code: string): number {
		if (/^game\.(bad_variable|unknown_variable)$/.test(code)) return 3;
		if (/^(game\.(bad_memory|bad_cpus|bad_image)|app\.(bad_name|exists|bad_domain|panel_domain))$/.test(code)) return 2;
		return step;
	}

	async function create() {
		problems = {};
		error = '';
		for (const v of editable) {
			if (!values[v.env].trim() && v.rules?.includes('required')) problems[v.env] = t('game.new.required', { name: v.name });
		}
		if (Object.keys(problems).length) {
			step = 3;
			return;
		}
		busy = true;
		try {
			await api('POST', '/apps/files', {
				name: name.trim(),
				runtime: chosen,
				image,
				memory_mb: memory,
				cpus,
				disk_mb: disk,
				domain: domain.trim(),
				// Only what was changed: the server has defaults of its own for the rest.
				variables: Object.fromEntries(editable.filter((v) => values[v.env] !== v.value).map((v) => [v.env, values[v.env]]))
			});
			await reload();
			await goto(`/a/${name.trim()}`);
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
		<AppIcon source="files" size="lg" />
		<Lead title={t('files.new.title')} text={' ' + t('files.new.lead')} />
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
				<h2 class="font-medium">{t('files.new.runtimeTitle')}</h2>
				<p class="text-sm text-muted">{t('files.new.runtimeLead')}</p>
			</div>
			<div class="grid gap-3 sm:grid-cols-2">
				{#each catalog as c (c.id)}
					<button
						type="button"
						class="flex items-start gap-4 rounded-2xl border p-4 text-left transition hover:bg-hover {chosen === c.id ? 'border-fg' : 'border-line'}"
						aria-pressed={chosen === c.id}
						onclick={() => (chosen = c.id)}
					>
						<span class="inline-flex size-10 shrink-0 items-center justify-center rounded-xl border border-line bg-panel" aria-hidden="true"><FolderCode size={20} strokeWidth={1.5} /></span>
						<span class="min-w-0">
							<span class="block font-medium">{runtimeLabel[c.id] ?? c.name}</span>
							<span class="block text-sm text-muted">{t(`files.runtime.${c.id}` as 'files.runtime.nodejs')}</span>
						</span>
					</button>
				{/each}
			</div>
			<ErrorText message={error} />
			<div class="flex items-center gap-3">
				<Button busy={loading} disabled={!chosen} onclick={pick}>{t('common.continue')}</Button>
				<Button kind="quiet" type="button" onclick={() => goto('/new')}>{t('new.back')}</Button>
			</div>
		</section>
	{:else if step === 2 && preview}
		<form
			class="flex flex-col gap-6"
			onsubmit={(e) => {
				e.preventDefault();
				if (editable.length) step = 3;
				else create();
			}}
		>
			<div>
				<h2 class="font-medium">{t('files.new.appTitle', { runtime: runtimeLabel[chosen] ?? preview.name })}</h2>
				<p class="text-sm text-muted">{t('files.new.appLead')}</p>
			</div>
			<Field
				label={t('new.name')}
				hint={t('new.nameHint')}
				pattern="[a-z0-9][a-z0-9\-]*"
				maxlength={40}
				required
				autocomplete="off"
				bind:value={name}
				oninput={() => (named = true)}
			/>
			<div class="flex flex-col gap-1.5">
				<Field label={t('new.domain')} placeholder="app.example.com" autocomplete="off" bind:value={domain} />
				<p class="text-sm text-muted">{t('files.new.domainHint')} {server.info?.access === 'tunnel' ? t('new.domainHintTunnel') : t('new.domainHint')}</p>
			</div>
			{#if preview.images.length > 1}
				<div class="flex flex-col gap-1.5">
					<label for="image" class="text-sm font-medium">{t('files.new.version', { runtime: runtimeLabel[chosen] ?? preview.name })}</label>
					<select id="image" class="h-10 w-full rounded-xl border border-line bg-bg px-3 text-[15px]" bind:value={image}>
						{#each preview.images as img (img.ref)}<option value={img.ref}>{img.label}</option>{/each}
					</select>
					<p class="text-sm text-muted">{t('files.new.versionHint')}</p>
				</div>
			{/if}
			<Resources bind:memory bind:cpus />
			<div class="flex flex-col gap-1.5">
				<label for="disk" class="text-sm font-medium">{t('files.new.disk')}</label>
				<select id="disk" class="h-10 w-40 rounded-xl border border-line bg-bg px-3 text-[15px]" bind:value={disk}>
					{#each sizeSteps(diskMB / 2, disk) as mb (mb)}<option value={mb}>{megabytes(mb)}</option>{/each}
				</select>
				<p class="text-sm text-muted">{t('files.new.diskHint')}</p>
			</div>
			<ErrorText message={error} />
			<div class="flex items-center gap-3">
				<Button type="submit" {busy}>{editable.length ? t('common.continue') : t('files.new.create')}</Button>
				<Button kind="quiet" type="button" onclick={() => ((error = ''), (step = 1))}>{t('new.back')}</Button>
			</div>
		</form>
	{:else if step === 3 && preview}
		<form
			class="flex flex-col gap-6"
			onsubmit={(e) => {
				e.preventDefault();
				create();
			}}
		>
			<div>
				<h2 class="font-medium">{t('files.new.settingsTitle')}</h2>
				<p class="text-sm text-muted">{t('files.new.settingsLead')}</p>
			</div>
			{#each editable as v (v.env)}
				<div class="flex flex-col gap-1.5">
					<Field label={v.name} hint={v.description} placeholder={v.default} autocomplete="off" bind:value={values[v.env]} oninput={() => delete problems[v.env]} />
					<ErrorText message={problems[v.env] ?? ''} />
				</div>
			{/each}
			<ErrorText message={error} />
			<div class="flex items-center gap-3">
				<Button type="submit" {busy}>{t('files.new.create')}</Button>
				<Button kind="quiet" type="button" onclick={() => ((error = ''), (step = 2))}>{t('new.back')}</Button>
			</div>
		</form>
	{/if}
</div>
