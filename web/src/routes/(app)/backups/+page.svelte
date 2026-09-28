<script lang="ts">
	import { onMount } from 'svelte';
	import { Archive, Cloud, FileKey, KeyRound, Lock, Search, TriangleAlert } from '@lucide/svelte';
	import { api } from '$lib/api';
	import { saveRecovery, type Offsite } from '$lib/backups';
	import { sensitive } from '$lib/confirm.svelte';
	import { messageOf } from '$lib/errors';
	import { engineLabel, type Engine } from '$lib/apps.svelte';
	import { date } from '$lib/format';
	import { list, t, type Key } from '$lib/i18n';
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

	// Backups another server left in the folder.
	type Found = {
		opens: boolean;
		backups: { app: string; name: string; bytes: number; created: string; engine: Engine | ''; volumes?: string[]; locked?: boolean }[];
	};
	let found = $state<Found | null>(null);
	let foundNote = $state('');
	const groups = $derived.by(() => {
		const by = new Map<string, Found['backups']>();
		for (const b of found?.backups ?? []) by.set(b.app, [...(by.get(b.app) ?? []), b]);
		return [...by].map(([app, l]) => ({ app, list: l }));
	});
	const kind = (b: Found['backups'][number]) =>
		b.locked ? '' : b.engine ? engineLabel[b.engine] : t('offsite.volumes', { dirs: list((b.volumes ?? []).map((v) => '/' + v)) });

	async function look() {
		busy = true;
		error = '';
		foundNote = '';
		try {
			found = await api<Found>('GET', '/offsite/found');
		} catch (err) {
			error = messageOf(err);
		} finally {
			busy = false;
		}
	}

	let keyFile = $state<HTMLInputElement>();
	async function addKey() {
		const file = keyFile?.files?.[0];
		if (!file) return;
		busy = true;
		error = '';
		try {
			const recovery = await file.text();
			const out = await sensitive(() => api<{ added: number }>('POST', '/backups/keys', { recovery }));
			foundNote = t('offsite.keyAdded', { n: out.added });
			found = await api<Found>('GET', '/offsite/found');
		} catch (err) {
			error = messageOf(err);
		} finally {
			busy = false;
			if (keyFile) keyFile.value = '';
		}
	}

	async function addFound() {
		busy = true;
		error = '';
		try {
			const out = await api<{ added: number }>('POST', '/offsite/found');
			found = await api<Found>('GET', '/offsite/found');
			foundNote = t('offsite.added', { n: out.added });
		} catch (err) {
			error = messageOf(err);
		} finally {
			busy = false;
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

		{#if data.set && !editing}
			<section class="flex flex-col gap-4 rounded-2xl border border-line p-5">
				<div class="flex items-start gap-4">
					<span class="grid size-10 shrink-0 place-items-center rounded-xl bg-selected"><Search size={20} strokeWidth={1.75} /></span>
					<div class="min-w-0">
						<h2 class="font-medium">{t('offsite.foundTitle')}</h2>
						<p class="text-sm text-muted">{t('offsite.foundLead')}</p>
					</div>
				</div>
				<div class="flex flex-col gap-4 sm:pl-14">
					{#if found && found.backups.length === 0}
						<p class="text-sm text-muted">{t('offsite.nothing', { prefix: data.prefix ?? '' })}</p>
					{:else if found}
						<ul class="flex flex-col divide-y divide-line rounded-2xl border border-line">
							{#each groups as g (g.app)}
								<li class="flex min-w-0 items-center gap-3 p-4">
									<span class="grid size-9 shrink-0 place-items-center rounded-xl bg-selected"
										>{#if g.list[0].locked}<Lock size={16} />{:else}<Archive size={18} strokeWidth={1.75} />{/if}</span
									>
									<div class="min-w-0">
										<p class="truncate text-[15px]">{g.app}{#if kind(g.list[0])}<span class="text-muted"> · {kind(g.list[0])}</span>{/if}</p>
										<p class="text-sm text-muted">{t('offsite.foundItem', { n: g.list.length, when: date(g.list[0].created) })}</p>
									</div>
								</li>
							{/each}
						</ul>
						{#if !found.opens}
							<div class="flex flex-col gap-3 rounded-xl border border-warn/40 bg-warn/5 p-4">
								<p class="flex items-start gap-2 text-sm"><FileKey size={16} class="mt-0.5 shrink-0 text-warn" />{t('offsite.locked')}</p>
								<label class="self-start">
									<span class="sr-only">{t('offsite.addKey')}</span>
									<input bind:this={keyFile} type="file" accept=".txt,text/plain" class="text-sm file:mr-3 file:h-9 file:rounded-full file:border-0 file:bg-selected file:px-4 file:text-sm file:text-fg" onchange={addKey} disabled={busy} />
								</label>
							</div>
						{:else}
							<Button class="self-start" {busy} onclick={addFound}>{t('offsite.addFound')}</Button>
						{/if}
					{/if}
					{#if foundNote}<p class="rounded-xl bg-ok/10 px-4 py-3 text-sm">{foundNote}</p>{/if}
					{#if !found}
						<Button kind="secondary" class="self-start" {busy} onclick={look}><Search size={16} strokeWidth={1.75} />{t('offsite.look')}</Button>
					{/if}
				</div>
			</section>
		{/if}

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
