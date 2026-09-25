<script lang="ts">
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { api } from '$lib/api';
	import { reload } from '$lib/apps.svelte';
	import { messageOf } from '$lib/errors';
	import { repositories, status, type Repository } from '$lib/github';
	import { t } from '$lib/i18n';
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
				cpus
			};
			if (github) Object.assign(body, { repo: cleanRepo, branch });
			else body.image = image;
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
	<div class="flex max-w-md flex-col gap-8">
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
			<details class="group rounded-xl border border-line px-4 py-3">
				<summary class="cursor-pointer text-sm font-medium select-none">{t('new.more')}</summary>
				<div class="mt-4"><Resources bind:memory bind:cpus /></div>
			</details>
			<ErrorText message={error} />
			<div class="mt-2 flex items-center gap-3">
				<Button type="submit" {busy}>{t('new.submit')}</Button>
				<Button kind="quiet" type="button" onclick={() => goto('/new')}>{t('new.back')}</Button>
			</div>
		</form>
	</div>
{/if}
