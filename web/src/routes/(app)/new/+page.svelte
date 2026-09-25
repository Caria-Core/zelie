<script lang="ts">
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { api } from '$lib/api';
	import { reload } from '$lib/apps.svelte';
	import { messageOf } from '$lib/errors';
	import { repositories, status, type Repository } from '$lib/github';
	import { t } from '$lib/i18n';
	import { Plus, X } from '@lucide/svelte';
	import { host, loadHost, megabytes } from '$lib/host.svelte';
	import { defaultSize, sizeSteps } from '$lib/volumes';
	import AppIcon from '$lib/ui/AppIcon.svelte';
	import Button from '$lib/ui/Button.svelte';
	import ErrorText from '$lib/ui/ErrorText.svelte';
	import Field from '$lib/ui/Field.svelte';
	import Lead from '$lib/ui/Lead.svelte';
	import Resources from '$lib/ui/Resources.svelte';
	import SourceChoice from '$lib/ui/SourceChoice.svelte';

	const source = $derived(page.url.searchParams.get('source'));
	const github = $derived(source === 'github');

	let id = $state('');
	let repo = $state('');
	let branch = $state('main');
	let image = $state('');
	let port = $state('');
	let domain = $state('');
	let memory = $state(512);
	let cpus = $state(1);
	// Everything else starts from sensible defaults and can wait for the
	// settings page; it is here for those who know what they want.
	let autoDeploy = $state(true);
	let restartPulls = $state(false);
	let buildCommand = $state('');
	let startCommand = $state('');
	let testCommand = $state('');
	let healthPath = $state('/');
	let volumes = $state<{ path: string; size: number }[]>([]);
	const diskMB = $derived(host.info ? host.info.disk_bytes / 2 ** 20 : 102400);
	$effect(() => {
		loadHost();
	});
	let error = $state('');
	let busy = $state(false);

	// With GitHub connected, the repositories Zelie was given are offered as
	// the name is typed, and picking one fills in its default branch.
	let connected = $state<boolean | null>(null);
	let repos = $state<Repository[]>([]);
	let branchTouched = $state(false);
	let asked = false;
	$effect(() => {
		if (!github || asked) return;
		asked = true;
		status()
			.then(async (s) => {
				connected = s.connected;
				// Without the connection, GitHub's page explains the setup first.
				// A public repository can still be deployed without it.
				if (!s.connected && !page.url.searchParams.has('public')) return goto('/github?from=new', { replaceState: true });
				if (s.connected) repos = await repositories();
			})
			.catch(() => {});
	});
	const cleanRepo = $derived(repo.trim().replace(/^https:\/\/github\.com\//, '').replace(/\.git$/, ''));
	$effect(() => {
		const r = repos.find((x) => x.full_name.toLowerCase() === cleanRepo.toLowerCase());
		if (r && !branchTouched) branch = r.default_branch;
	});

	// Suggest a name from the repository or image as it is typed.
	let named = $state(false);
	$effect(() => {
		if (named) return;
		const from = github ? repo.split('/')[1] ?? '' : (image.split('/').pop() ?? '').split(':')[0];
		id = from
			.toLowerCase()
			.replace(/[^a-z0-9-]+/g, '-')
			.replace(/^-+|-+$/g, '')
			.slice(0, 40);
	});

	async function submit(e: SubmitEvent) {
		e.preventDefault();
		busy = true;
		error = '';
		try {
			const body: Record<string, unknown> = {
				id,
				source,
				port: Number(port || (github ? 3000 : 80)),
				domain,
				memory_mb: memory,
				cpus,
				health_path: healthPath,
				start_command: startCommand,
				volumes: volumes.filter((v) => v.path.trim()).map((v) => ({ path: v.path.trim(), limit_mb: v.size }))
			};
			if (github) {
				Object.assign(body, { repo: cleanRepo, branch, auto_deploy: autoDeploy, restart_pulls: restartPulls, build_command: buildCommand });
				// Left empty, the first build suggests one.
				if (testCommand.trim()) body.test_command = testCommand;
			} else body.image = image;
			await api('POST', '/apps', body);
			await reload();
			await goto('/a/' + id);
		} catch (err) {
			error = messageOf(err);
		} finally {
			busy = false;
		}
	}
</script>

{#if source !== 'github' && source !== 'image'}
	<div class="flex max-w-2xl flex-col gap-8">
		<Lead title={t('new.title')} text={' ' + t('new.lead')} />
		<SourceChoice />
	</div>
{:else}
	<div class="flex max-w-xl flex-col gap-8">
		<div class="flex items-center gap-4">
			<AppIcon source={source} size="lg" />
			<Lead title={github ? t('source.github') + '.' : t('source.image') + '.'} />
		</div>
		<form class="flex flex-col gap-4" onsubmit={submit}>
			{#if github}
				<div class="flex flex-col gap-1.5">
					<Field label={t('new.repo')} placeholder="owner/name" required autocomplete="off" list="repos" bind:value={repo} />
					<datalist id="repos">
						{#each repos as r (r.full_name)}<option value={r.full_name}></option>{/each}
					</datalist>
					<p class="text-sm text-muted">
						{#if connected}
							{t('new.repoHintConnected')}
						{:else}
							{t('new.repoHint')}
							{#if connected === false}<a href="/github" class="text-fg underline underline-offset-2">{t('new.repoHintConnect')}</a>{/if}
						{/if}
					</p>
				</div>
				<Field label={t('new.branch')} required autocomplete="off" bind:value={branch} oninput={() => (branchTouched = true)} />
			{:else}
				<Field label={t('new.image')} placeholder="nginx:alpine" required autocomplete="off" bind:value={image} />
			{/if}
			<Field
				label={t('new.name')}
				hint={t('new.nameHint')}
				pattern="[a-z0-9][a-z0-9\-]*"
				maxlength={40}
				required
				autocomplete="off"
				bind:value={id}
				oninput={() => (named = true)}
			/>
			<div class="grid grid-cols-[7rem_1fr] gap-4">
				<Field label={t('new.port')} type="number" min="1" max="65535" placeholder={github ? '3000' : '80'} bind:value={port} />
				<Field label={t('new.domain')} placeholder="app.example.com" autocomplete="off" bind:value={domain} />
			</div>
			<p class="-mt-2 text-sm text-muted">{t('new.portHint')} {t('new.domainHint')}</p>
			<section class="mt-4 flex flex-col gap-4">
				<div>
					<h2 class="font-medium">{t('new.more')}</h2>
					<p class="text-sm text-muted">{t('new.resourcesLead')}</p>
				</div>
				<Resources bind:memory bind:cpus />
			</section>
			<details class="group mt-2 rounded-xl border border-line">
				<summary class="flex cursor-pointer items-center gap-2 px-4 py-3 text-[15px] font-medium select-none">
					<Plus size={16} class="transition group-open:rotate-45" />{t('new.advanced')}
					<span class="font-normal text-muted">{t('new.advancedLead')}</span>
				</summary>
				<div class="flex flex-col gap-4 border-t border-line px-4 py-4">
					{#if github}
						<label class="flex items-start gap-2.5 text-[15px]">
							<input type="checkbox" class="mt-1" bind:checked={autoDeploy} />
							<span>{t('settings.autoDeploy')}<span class="block text-sm text-muted">{t('settings.autoDeployHint')}</span></span>
						</label>
						<label class="flex items-start gap-2.5 text-[15px]">
							<input type="checkbox" class="mt-1" bind:checked={restartPulls} />
							<span>{t('settings.restartPulls')}<span class="block text-sm text-muted">{t('settings.restartPullsHint')}</span></span>
						</label>
						<Field label={t('settings.build')} placeholder={t('new.buildAuto')} autocomplete="off" bind:value={buildCommand} />
					{/if}
					<Field
						label={t('settings.start')}
						placeholder={github ? t('new.startAuto') : t('settings.startDefault')}
						autocomplete="off"
						bind:value={startCommand}
					/>
					{#if github}
						<Field label={t('settings.tests')} placeholder={t('new.testAuto')} autocomplete="off" bind:value={testCommand} />
					{/if}
					<Field label={t('settings.health')} hint={t('settings.healthHint')} required autocomplete="off" bind:value={healthPath} />
					<div class="flex flex-col gap-2">
						<span class="text-sm font-medium">{t('new.volumes')}</span>
						{#each volumes as v, i (i)}
							<div class="flex items-center gap-2">
								<input
									class="h-10 min-w-0 flex-1 rounded-xl border border-line bg-bg px-3.5 font-mono text-[15px] outline-none focus:border-muted"
									aria-label={t('storage.path')}
									placeholder="/data"
									autocomplete="off"
									bind:value={v.path}
								/>
								<select class="h-10 rounded-xl border border-line bg-bg px-2 text-[15px]" aria-label={t('storage.limit')} bind:value={v.size}>
									{#each sizeSteps(diskMB) as mb (mb)}<option value={mb}>{megabytes(mb)}</option>{/each}
								</select>
								<Button kind="quiet" type="button" class="!px-2" aria-label={t('new.removeVolume')} onclick={() => volumes.splice(i, 1)}
									><X size={16} /></Button
								>
							</div>
						{/each}
						<p class="text-sm text-muted">{t('new.volumesHint')}</p>
						<Button kind="secondary" type="button" class="!h-8 self-start !px-3.5 text-sm" onclick={() => volumes.push({ path: '', size: defaultSize })}
							><Plus size={14} />{t('new.addVolume')}</Button
						>
					</div>
				</div>
			</details>
			<ErrorText message={error} />
			<div class="mt-2 flex items-center gap-3">
				<Button type="submit" {busy}>{t('new.submit')}</Button>
				<Button kind="quiet" type="button" onclick={() => goto('/new')}>{t('new.back')}</Button>
			</div>
		</form>
	</div>
{/if}
