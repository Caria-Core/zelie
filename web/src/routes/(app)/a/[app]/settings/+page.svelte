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

	const app = $derived(current.app!);
	let form = $state({ repo: '', branch: '', image: '', port: '', domain: '', memory: '', cpus: '', autoDeploy: true });
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
			memory: String(app.memory_mb),
			cpus: String(app.cpus),
			autoDeploy: app.auto_deploy
		};
	});

	async function save(e: SubmitEvent) {
		e.preventDefault();
		busy = true;
		error = '';
		saved = false;
		try {
			const body: Record<string, unknown> = {
				port: Number(form.port),
				domain: form.domain,
				memory_mb: Number(form.memory),
				cpus: Number(form.cpus)
			};
			if (app.source === 'github') Object.assign(body, { repo: form.repo, branch: form.branch, auto_deploy: form.autoDeploy });
			else body.image = form.image;
			await api('PATCH', `/apps/${app.id}`, body);
			await Promise.all([load(app.id), reload()]);
			saved = true;
		} catch (err) {
			error = messageOf(err);
		} finally {
			busy = false;
		}
	}

	async function remove() {
		if (!confirm(t('settings.deleteConfirm', { id: app.id }))) return;
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

<div class="flex max-w-md flex-col gap-10">
	<form class="flex flex-col gap-4" onsubmit={save}>
		{#if app.source === 'github'}
			<Field label={t('new.repo')} required autocomplete="off" bind:value={form.repo} />
			<Field label={t('new.branch')} required autocomplete="off" bind:value={form.branch} />
			<label class="flex items-start gap-2.5 text-[15px]">
				<input type="checkbox" class="mt-1" bind:checked={form.autoDeploy} />
				<span>{t('settings.autoDeploy')}<span class="block text-sm text-muted">{t('settings.autoDeployHint')}</span></span>
			</label>
		{:else}
			<Field label={t('new.image')} required autocomplete="off" bind:value={form.image} />
		{/if}
		<div class="grid grid-cols-[7rem_1fr] gap-4">
			<Field label={t('new.port')} type="number" min="1" max="65535" required bind:value={form.port} />
			<Field label={t('new.domain')} placeholder="app.example.com" autocomplete="off" bind:value={form.domain} />
		</div>
		<div class="grid grid-cols-2 gap-4">
			<Field label={t('new.memory')} type="number" min="16" step="16" required bind:value={form.memory} />
			<Field label={t('new.cpus')} type="number" min="0.1" step="0.1" required bind:value={form.cpus} />
		</div>
		<ErrorText message={error} />
		<div class="flex items-center gap-3">
			<Button type="submit" {busy}>{t('settings.save')}</Button>
			{#if saved}<span role="status" class="text-sm text-ok">{t('settings.saved')}</span>{/if}
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
