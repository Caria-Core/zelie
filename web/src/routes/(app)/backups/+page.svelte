<script lang="ts">
	import { onMount } from 'svelte';
	import { Cloud, KeyRound, Lock, TriangleAlert } from '@lucide/svelte';
	import { api } from '$lib/api';
	import { saveRecovery, type Offsite } from '$lib/backups';
	import { sensitive } from '$lib/confirm.svelte';
	import { messageOf } from '$lib/errors';
	import { date } from '$lib/format';
	import { t, type Key } from '$lib/i18n';
	import Button from '$lib/ui/Button.svelte';
	import ErrorText from '$lib/ui/ErrorText.svelte';
	import Field from '$lib/ui/Field.svelte';
	import Lead from '$lib/ui/Lead.svelte';

	type State = Offsite & { recovery_saved_at: string | null };
	let data = $state<State | null>(null);
	let editing = $state(false);
	let busy = $state(false);
	let error = $state('');
	let notice = $state('');

	// What each provider's address looks like, and where its keys are.
	const providers = [
		{ id: 'r2', name: 'Cloudflare R2', endpoint: 'https://<account-id>.r2.cloudflarestorage.com', region: 'auto' },
		{ id: 'b2', name: 'Backblaze B2', endpoint: 'https://s3.<region>.backblazeb2.com', region: 'eu-central-003' },
		{ id: 'aws', name: 'Amazon S3', endpoint: 'https://s3.<region>.amazonaws.com', region: 'eu-central-1' },
		{ id: 'wasabi', name: 'Wasabi', endpoint: 'https://s3.<region>.wasabisys.com', region: 'eu-central-1' },
		{ id: 'other', name: '', endpoint: 'https://s3.example.com', region: '' }
	] as const;
	let provider = $state<(typeof providers)[number]['id']>('r2');
	const chosen = $derived(providers.find((p) => p.id === provider)!);

	let form = $state({ endpoint: '', region: '', bucket: '', prefix: '', access_key: '', secret_key: '' });
	const plain = $derived(form.endpoint.trim().toLowerCase().startsWith('http://'));

	onMount(load);

	async function load() {
		try {
			data = await api<State>('GET', '/offsite');
		} catch (err) {
			error = messageOf(err);
		}
	}

	function edit() {
		form = {
			endpoint: data?.endpoint ?? '',
			region: data?.region ?? '',
			bucket: data?.bucket ?? '',
			prefix: data?.prefix ?? '',
			access_key: data?.access_key ?? '',
			secret_key: ''
		};
		provider = guess(form.endpoint);
		notice = '';
		editing = true;
	}

	function guess(endpoint: string): typeof provider {
		if (endpoint.includes('r2.cloudflarestorage.com')) return 'r2';
		if (endpoint.includes('backblazeb2.com')) return 'b2';
		if (endpoint.includes('amazonaws.com')) return 'aws';
		if (endpoint.includes('wasabisys.com')) return 'wasabi';
		return endpoint ? 'other' : 'r2';
	}

	async function save(e: SubmitEvent) {
		e.preventDefault();
		busy = true;
		error = '';
		try {
			await sensitive(() => api('PUT', '/offsite', form));
			await load();
			editing = false;
			notice = t('offsite.saved');
		} catch (err) {
			error = messageOf(err);
		} finally {
			busy = false;
		}
	}

	async function remove() {
		if (!confirm(t('offsite.removeConfirm'))) return;
		busy = true;
		error = '';
		try {
			await sensitive(() => api('DELETE', '/offsite'));
			notice = '';
			await load();
		} catch (err) {
			error = messageOf(err);
		} finally {
			busy = false;
		}
	}

	async function recovery() {
		error = '';
		try {
			await saveRecovery();
			await load();
		} catch (err) {
			error = messageOf(err);
		}
	}

	const chip = 'rounded-full border px-3 py-1 text-sm transition';
</script>

<div class="flex max-w-2xl flex-col gap-8">
	<Lead title={t('offsite.title')} text={' ' + t('offsite.lead')} />

	{#if data}
		<section class="flex flex-col gap-5 rounded-2xl border border-line p-5">
			<div class="flex items-start gap-4">
				<span class="grid size-10 shrink-0 place-items-center rounded-xl {data.set ? 'bg-ok/10 text-ok' : 'bg-selected'}"
					><Cloud size={20} strokeWidth={1.75} /></span
				>
				<div class="min-w-0">
					<h2 class="font-medium">{t('offsite.storageTitle')}</h2>
					<p class="text-sm text-muted">{t('offsite.storageLead')}</p>
				</div>
			</div>

			{#if data.set && !editing}
				<div class="flex flex-col gap-3 sm:pl-14">
					<div class="flex flex-col gap-1 text-sm">
						<p class="break-words text-[15px]">{t('offsite.where', { bucket: data.bucket ?? '', endpoint: data.endpoint ?? '' })}</p>
						<p class="break-all text-muted">{t('offsite.folder', { prefix: data.prefix ?? '' })}</p>
						<p class="break-all text-muted">{t('offsite.key', { key: data.access_key ?? '' })}</p>
					</div>
					<p class="flex items-start gap-2 text-sm text-muted"><Lock size={14} class="mt-[3px] shrink-0" />{t('offsite.encrypted')}</p>
					{#if notice}<p class="rounded-xl bg-ok/10 px-4 py-3 text-sm">{notice}</p>{/if}
					<ErrorText message={error} />
					<div class="flex flex-wrap gap-2">
						<Button kind="secondary" onclick={edit} disabled={busy}>{t('offsite.change')}</Button>
						<Button kind="quiet" class="hover:!text-danger" {busy} onclick={remove}>{t('offsite.remove')}</Button>
					</div>
				</div>
			{:else if !data.set && !editing}
				<div class="flex flex-col gap-3 sm:pl-14">
					<p class="flex items-start gap-2 text-sm text-muted"><Lock size={14} class="mt-[3px] shrink-0" />{t('offsite.encrypted')}</p>
					<Button class="self-start" onclick={edit}>{t('backups.offsite.setUp')}</Button>
				</div>
			{:else}
				<form class="flex flex-col gap-4 sm:pl-14" onsubmit={save}>
					<fieldset class="flex flex-col gap-2">
						<legend class="mb-2 text-sm font-medium">{t('offsite.provider')}</legend>
						<div class="flex flex-wrap gap-2">
							{#each providers as p (p.id)}
								<button
									type="button"
									class="{chip} {provider === p.id ? 'border-fg bg-fg text-bg' : 'border-line text-muted hover:text-fg'}"
									aria-pressed={provider === p.id}
									onclick={() => (provider = p.id)}>{p.name || t('offsite.other')}</button
								>
							{/each}
						</div>
						<p class="text-sm text-muted">{t(`offsite.hint.${provider}` as Key)}</p>
					</fieldset>
					<Field label={t('offsite.endpoint')} required autocomplete="off" inputmode="url" placeholder={chosen.endpoint} bind:value={form.endpoint} />
					{#if plain}
						<p class="-mt-2 flex items-start gap-2 text-sm text-warn"><TriangleAlert size={14} class="mt-[3px] shrink-0" />{t('offsite.http')}</p>
					{/if}
					<div class="grid gap-4 sm:grid-cols-2">
						<Field label={t('offsite.bucket')} hint={t('offsite.bucketHint')} required autocomplete="off" bind:value={form.bucket} />
						<Field label={t('offsite.region')} hint={t('offsite.regionHint')} autocomplete="off" placeholder={chosen.region} bind:value={form.region} />
					</div>
					<Field label={t('offsite.prefix')} hint={t('offsite.prefixHint')} autocomplete="off" bind:value={form.prefix} />
					<div class="grid gap-4 sm:grid-cols-2">
						<Field label={t('offsite.accessKey')} required autocomplete="off" spellcheck="false" bind:value={form.access_key} />
						<Field
							label={t('offsite.secretKey')}
							type="password"
							required={!data.set}
							hint={data.set ? t('offsite.secretKeep') : ''}
							autocomplete="new-password"
							bind:value={form.secret_key}
						/>
					</div>
					<p class="flex items-start gap-2 text-sm text-muted"><KeyRound size={14} class="mt-[3px] shrink-0" />{t('offsite.keysHint')}</p>
					<ErrorText message={error} />
					<div class="flex flex-wrap items-center gap-3">
						<Button type="submit" {busy}>{t('offsite.save')}</Button>
						{#if data.set}<Button kind="quiet" disabled={busy} onclick={() => ((editing = false), (error = ''))}>{t('common.cancel')}</Button>{/if}
					</div>
					<p class="text-sm text-muted">{t('offsite.saveHint')}</p>
				</form>
			{/if}
		</section>

		<section class="flex flex-col gap-4 rounded-2xl border p-5 {data.recovery_saved_at ? 'border-line' : 'border-warn/40 bg-warn/5'}">
			<div class="flex items-start gap-4">
				<span class="grid size-10 shrink-0 place-items-center rounded-xl {data.recovery_saved_at ? 'bg-selected' : 'bg-warn/15 text-warn'}"
					><KeyRound size={20} strokeWidth={1.75} /></span
				>
				<div class="min-w-0">
					<h2 class="font-medium">{t('offsite.recoveryTitle')}</h2>
					<p class="text-sm text-muted">{t('offsite.recoveryLead')}</p>
				</div>
			</div>
			<div class="flex flex-wrap items-center gap-3 sm:pl-14">
				<Button kind="secondary" onclick={recovery}>{data.recovery_saved_at ? t('backups.recoveryAgain') : t('backups.recoverySave')}</Button>
				<p class="text-sm text-muted">
					{data.recovery_saved_at ? t('backups.recoverySaved', { when: date(data.recovery_saved_at) }) : t('offsite.recoveryNever')}
				</p>
			</div>
		</section>
	{:else}
		<ErrorText message={error} />
	{/if}
</div>
