<script lang="ts">
	import { Trash } from '@lucide/svelte';
	import { goto } from '$app/navigation';
	import { api } from '$lib/api';
	import { reload } from '$lib/apps.svelte';
	import { current, load } from '$lib/current.svelte';
	import { messageOf } from '$lib/errors';
	import { t } from '$lib/i18n';
	import Button from '$lib/ui/Button.svelte';
	import ErrorText from '$lib/ui/ErrorText.svelte';
	import Field from '$lib/ui/Field.svelte';
	import Resources from '$lib/ui/Resources.svelte';
	import type { Usage } from '$lib/host.svelte';

	const app = $derived(current.app!);
	let form = $state({
		repo: '',
		branch: '',
		image: '',
		port: '',
		domain: '',
		memory: 512,
		cpus: 1,
		autoDeploy: true,
		restartPulls: false,
		health: '/',
		tests: '',
		build: '',
		start: ''
	});
	const github = $derived(app.source === 'github');
	// Only a database's resources can change; the rest is Zelie's.
	const database = $derived(!!app.engine);
	let error = $state('');
	let saved = $state(false);
	let busy = $state(false);

	let loadedFor = '';
	$effect(() => {
		if (app.id === loadedFor) return;
		loadedFor = app.id;
		form = {
			repo: app.repo ?? '',
			branch: app.branch ?? '',
			image: app.image ?? '',
			port: String(app.port),
			domain: app.domain ?? '',
			memory: app.memory_mb,
			cpus: app.cpus,
			autoDeploy: app.auto_deploy,
			restartPulls: app.restart_pulls,
			health: app.health_path,
			tests: app.test_command,
			build: app.build_command,
			start: app.start_command
		};
	});

	async function save(e: SubmitEvent) {
		e.preventDefault();
		busy = true;
		error = '';
		saved = false;
		try {
			const body: Record<string, unknown> = { memory_mb: form.memory, cpus: form.cpus };
			if (!database) {
				Object.assign(body, { port: Number(form.port), domain: form.domain, health_path: form.health, start_command: form.start });
				if (github)
					Object.assign(body, {
						repo: form.repo,
						branch: form.branch,
						auto_deploy: form.autoDeploy,
						restart_pulls: form.restartPulls,
						test_command: form.tests,
						build_command: form.build
					});
				else body.image = form.image;
			}
			await api('PATCH', `/apps/${app.id}`, body);
			await Promise.all([load(app.id), reload()]);
			saved = true;
		} catch (err) {
			error = messageOf(err);
		} finally {
			busy = false;
		}
	}

	// What the app uses now, refreshed while the page is open.
	let usage = $state<Usage | null>(null);
	$effect(() => {
		const id = app.id;
		let stop = false;
		const tick = async () => {
			if (stop) return;
			if (document.visibilityState === 'visible') usage = await api<Usage>('GET', `/apps/${id}/usage`).catch(() => null);
			setTimeout(tick, 3000);
		};
		tick();
		return () => (stop = true);
	});

	async function remove() {
		if (!confirm(t(database ? 'settings.deleteDatabaseConfirm' : 'settings.deleteConfirm', { id: app.id }))) return;
		busy = true;
		try {
			await api('DELETE', `/apps/${app.id}`);
			await reload();
			await goto('/');
		} catch (err) {
			error = messageOf(err);
			busy = false;
		}
	}
</script>

{#snippet check(label: string, hint: string, checked: boolean, set: (v: boolean) => void)}
	<label class="flex items-start gap-2.5 text-[15px]">
		<input type="checkbox" class="mt-1" {checked} onchange={(e) => set(e.currentTarget.checked)} />
		<span>{label}<span class="block text-sm text-muted">{hint}</span></span>
	</label>
{/snippet}

{#snippet heading(title: string, lead: string)}
	<div>
		<h2 class="font-medium">{title}</h2>
		<p class="text-sm text-muted">{lead}</p>
	</div>
{/snippet}

<div class="flex max-w-xl flex-col gap-10">
	<form class="flex flex-col gap-10" onsubmit={save}>
		{#if !database}
			<section class="flex flex-col gap-4">
				{@render heading(t('settings.source'), github ? t('settings.sourceLeadGithub') : t('settings.sourceLeadImage'))}
				{#if github}
					<div class="grid gap-4 sm:grid-cols-[1fr_12rem]">
						<Field label={t('new.repo')} required autocomplete="off" bind:value={form.repo} />
						<Field label={t('new.branch')} required autocomplete="off" bind:value={form.branch} />
					</div>
					{@render check(t('settings.autoDeploy'), t('settings.autoDeployHint'), form.autoDeploy, (v) => (form.autoDeploy = v))}
					{@render check(t('settings.restartPulls'), t('settings.restartPullsHint'), form.restartPulls, (v) => (form.restartPulls = v))}
				{:else}
					<Field label={t('new.image')} required autocomplete="off" bind:value={form.image} />
			{/if}
		</section>

		<section class="flex flex-col gap-4">
			{@render heading(t('settings.commands'), github ? t('settings.commandsLeadGithub') : t('settings.commandsLeadImage'))}
			{#if github && app.detected.builder !== 'dockerfile'}
				<Field
					label={t('settings.build')}
					placeholder={app.detected.build ||
						(app.detected.builder === 'railpack' ? t('settings.buildNone') : t('settings.buildDefault'))}
					autocomplete="off"
					bind:value={form.build}
				/>
			{/if}
			<Field
				label={t('settings.start')}
				placeholder={app.detected.start || t('settings.startDefault')}
				autocomplete="off"
				bind:value={form.start}
			/>
			{#if github}
				<Field label={t('settings.tests')} hint={t('settings.testsHint')} placeholder="npm test" autocomplete="off" bind:value={form.tests} />
			{/if}
		</section>

		<section class="flex flex-col gap-4">
			{@render heading(t('settings.network'), t('settings.networkLead'))}
			<div class="grid grid-cols-[7rem_1fr] gap-4">
				<Field label={t('new.port')} type="number" min="1" max="65535" required bind:value={form.port} />
				<Field label={t('new.domain')} placeholder="app.example.com" autocomplete="off" bind:value={form.domain} />
			</div>
			<Field label={t('settings.health')} hint={t('settings.healthHint')} required autocomplete="off" bind:value={form.health} />
		</section>
		{/if}

		<section class="flex flex-col gap-4">
			{@render heading(t('new.more'), database ? t('db.resourcesLead') : t('settings.resourcesLead'))}
			<Resources bind:memory={form.memory} bind:cpus={form.cpus} app={app.id} {usage} />
		</section>

		<div class="sticky bottom-0 -mx-1 flex items-center gap-3 bg-bg/90 px-1 py-3 backdrop-blur">
			<Button type="submit" {busy}>{t('settings.save')}</Button>
			{#if saved}<span role="status" class="text-sm text-ok">{t('settings.saved')}</span>{/if}
			<ErrorText message={error} />
		</div>
	</form>

	<section class="flex flex-col gap-3 rounded-2xl border border-danger/30 p-5">
		<div>
			<h2 class="font-medium">{t('settings.danger')}</h2>
			<p class="text-sm text-muted">{t('settings.dangerLead')}</p>
		</div>
		<Button kind="secondary" class="self-start hover:!text-danger" {busy} onclick={remove}><Trash size={16} />{t('settings.delete')}</Button>
	</section>
</div>
