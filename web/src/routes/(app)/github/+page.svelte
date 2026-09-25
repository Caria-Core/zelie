<script lang="ts">
	import { onMount } from 'svelte';
	import { ExternalLink } from '@lucide/svelte';
	import { api } from '$lib/api';
	import { sensitive } from '$lib/confirm.svelte';
	import { messageOf } from '$lib/errors';
	import { ago } from '$lib/format';
	import { createApp, status, type GitHub } from '$lib/github';
	import { t } from '$lib/i18n';
	import Button from '$lib/ui/Button.svelte';
	import ErrorText from '$lib/ui/ErrorText.svelte';
	import Field from '$lib/ui/Field.svelte';
	import Lead from '$lib/ui/Lead.svelte';

	let gh = $state<GitHub | null>(null);
	let owner = $state<'personal' | 'org'>('personal');
	let org = $state('');
	let busy = $state(false);
	let error = $state('');

	onMount(load);

	async function load() {
		try {
			gh = await status();
		} catch (err) {
			error = messageOf(err);
		}
	}

	async function connect(e: SubmitEvent) {
		e.preventDefault();
		busy = true;
		error = '';
		try {
			const out = await sensitive(() =>
				api<{ action: string; manifest: string }>('POST', '/github/manifest', { org: owner === 'org' ? org : '' })
			);
			createApp(out.action, out.manifest);
		} catch (err) {
			error = messageOf(err);
			busy = false;
		}
	}

	async function disconnect() {
		if (!confirm(t('github.disconnectConfirm'))) return;
		busy = true;
		error = '';
		try {
			await sensitive(() => api('DELETE', '/github'));
			await load();
		} catch (err) {
			error = messageOf(err);
		} finally {
			busy = false;
		}
	}

	const radio = 'flex items-center gap-2 text-[15px]';
</script>

<div class="flex max-w-xl flex-col gap-8">
	<Lead title={t('github.title')} text={' ' + t('github.lead')} />

	{#if gh && !gh.connected}
		<form class="flex flex-col gap-5" onsubmit={connect}>
			<p class="text-muted">{t('github.explain')}</p>
			<fieldset class="flex flex-col gap-2">
				<legend class="mb-2 text-sm font-medium">{t('github.where')}</legend>
				<label class={radio}><input type="radio" value="personal" bind:group={owner} />{t('github.personal')}</label>
				<label class={radio}><input type="radio" value="org" bind:group={owner} />{t('github.org')}</label>
			</fieldset>
			{#if owner === 'org'}
				<Field label={t('github.orgName')} required autocomplete="off" bind:value={org} />
			{/if}
			<ErrorText message={error} />
			<Button type="submit" class="self-start" {busy}>{t('github.connect')}</Button>
		</form>
	{:else if gh}
		<section class="flex flex-col gap-4 rounded-2xl border border-line p-5">
			<div class="flex flex-wrap items-center justify-between gap-3">
				<p>
					{t('github.app')} <span class="font-medium">{gh.slug}</span>
					<span class="text-muted">{t('github.owner', { owner: gh.owner ?? '' })}</span>
				</p>
				<a href={gh.html_url} target="_blank" rel="noreferrer" class="inline-flex items-center gap-1 text-sm text-muted hover:text-fg"
					>{t('github.onGitHub')}<ExternalLink size={14} /></a
				>
			</div>

			{#if gh.error}
				<p class="text-sm text-danger">{gh.error}</p>
			{:else}
				<div class="flex flex-col gap-1">
					<p class="text-sm font-medium">{t('github.access')}</p>
					{#each gh.installations ?? [] as i (i.id)}
						<p class="text-sm text-muted">{i.account} · {i.all ? t('github.allRepos') : t('github.someRepos')}</p>
					{:else}
						<p class="text-sm text-muted">{t('github.noAccess')}</p>
					{/each}
				</div>
				<a
					href={gh.install_url}
					class="inline-flex h-10 items-center self-start rounded-full bg-selected px-5 text-[15px] font-medium transition hover:bg-line"
					>{t('github.choose')}</a
				>

				{#if gh.webhook?.state === 'ok'}
					<p class="text-sm text-ok">{t('github.hookOk', { when: ago(gh.webhook.last!) })}</p>
				{:else if gh.webhook?.state === 'failing'}
					<p class="text-sm text-danger">{t('github.hookFailing', { status: gh.webhook.status ?? '' })}</p>
				{:else}
					<p class="text-sm text-muted">{t('github.hookNone')}</p>
				{/if}
			{/if}
		</section>

		<section class="flex flex-col gap-3">
			<p class="text-sm text-muted">{t('github.disconnectLead')}</p>
			<ErrorText message={error} />
			<Button kind="secondary" class="self-start hover:!text-danger" {busy} onclick={disconnect}>{t('github.disconnect')}</Button>
		</section>
	{:else}
		<ErrorText message={error} />
	{/if}
</div>
