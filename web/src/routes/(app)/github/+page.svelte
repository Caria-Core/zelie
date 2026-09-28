<script lang="ts">
	import { onMount } from 'svelte';
	import { Check, ExternalLink, FolderGit2, KeyRound, Rocket } from '@lucide/svelte';
	import { page } from '$app/state';
	import { api } from '$lib/api';
	import { sensitive } from '$lib/confirm.svelte';
	import { messageOf } from '$lib/errors';
	import { ago } from '$lib/format';
	import { createApp, status, type GitHub } from '$lib/github';
	import { say, t } from '$lib/i18n';
	import Button from '$lib/ui/Button.svelte';
	import ErrorText from '$lib/ui/ErrorText.svelte';
	import Field from '$lib/ui/Field.svelte';
	import Lead from '$lib/ui/Lead.svelte';

	let gh = $state<GitHub | null>(null);
	let owner = $state<'personal' | 'org'>('personal');
	let org = $state('');
	let busy = $state(false);
	let error = $state('');

	// Sent here from "New app" because GitHub is not connected yet.
	const fromNew = $derived(page.url.searchParams.get('from') === 'new');
	const installed = $derived((gh?.installations?.length ?? 0) > 0);
	// Which step is next: 1 create the App, 2 give it repositories, 3 deploy.
	const step = $derived(!gh?.connected ? 1 : !installed ? 2 : 3);

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
	const link =
		'inline-flex h-10 items-center gap-2 self-start rounded-full px-5 text-[15px] font-medium transition';
</script>

{#snippet stepHead(n: number, title: string, lead: string, Icon: typeof KeyRound)}
	{@const done = step > n}
	{@const now = step === n}
	<div class="flex items-start gap-4">
		<span
			class="flex size-10 shrink-0 items-center justify-center rounded-xl border {done
				? 'border-ok/40 bg-ok/10 text-ok'
				: now
					? 'border-fg bg-fg text-bg'
					: 'border-line text-muted'}"
			aria-hidden="true"
		>
			{#if done}<Check size={18} />{:else}<Icon size={18} strokeWidth={1.75} />{/if}
		</span>
		<div class="min-w-0 pt-0.5">
			<p class="text-sm text-muted">{t('github.step', { n })}{done ? ' · ' + t('github.done') : ''}</p>
			<h2 class="font-medium {now ? '' : 'text-fg/80'}">{title}</h2>
			<p class="mt-1 text-sm text-muted">{lead}</p>
		</div>
	</div>
{/snippet}

<div class="flex max-w-2xl flex-col gap-8">
	<Lead title={t('github.title')} text={' ' + t('github.lead')} />

	{#if fromNew && step < 3}
		<p class="rounded-xl border border-line bg-panel px-4 py-3 text-sm">
			{t('github.fromNew')}
			<a href="/new?source=github&public=1" class="text-muted underline underline-offset-2 hover:text-fg">{t('github.publicInstead')}</a>
		</p>
	{/if}

	{#if gh}
		<ol class="flex flex-col divide-y divide-line rounded-2xl border border-line">
			<!-- 1. The App -->
			<li class="flex flex-col gap-5 p-5">
				{@render stepHead(1, t('github.s1.title'), t('github.s1.lead'), KeyRound)}
				{#if !gh.connected}
					<form class="flex flex-col gap-4 sm:pl-14" onsubmit={connect}>
						<ul class="flex flex-col gap-1.5 text-sm">
							<li class="flex gap-2"><Check size={16} class="mt-0.5 shrink-0 text-ok" />{t('github.can.read')}</li>
							<li class="flex gap-2"><Check size={16} class="mt-0.5 shrink-0 text-ok" />{t('github.can.status')}</li>
							<li class="flex gap-2"><Check size={16} class="mt-0.5 shrink-0 text-ok" />{t('github.can.keys')}</li>
						</ul>
						<fieldset class="flex flex-col gap-2">
							<legend class="mb-2 text-sm font-medium">{t('github.where')}</legend>
							<label class={radio}><input type="radio" value="personal" bind:group={owner} />{t('github.personal')}</label>
							<label class={radio}><input type="radio" value="org" bind:group={owner} />{t('github.org')}</label>
						</fieldset>
						{#if owner === 'org'}
							<Field label={t('github.orgName')} required autocomplete="off" bind:value={org} />
						{/if}
						<ErrorText message={error} />
						<Button type="submit" class="self-start" {busy}>{t('github.connect')}<ExternalLink size={16} /></Button>
						<p class="text-sm text-muted">{t('github.s1.next')}</p>
					</form>
				{:else}
					<div class="flex flex-wrap items-center justify-between gap-3 sm:pl-14">
						<p class="text-sm">
							<span class="font-medium">{gh.slug}</span>
							<span class="text-muted">{t('github.owner', { owner: gh.owner ?? '' })}</span>
						</p>
						<a href={gh.html_url} target="_blank" rel="noreferrer" class="inline-flex items-center gap-1 text-sm text-muted hover:text-fg"
							>{t('github.onGitHub')}<ExternalLink size={14} /></a
						>
					</div>
				{/if}
			</li>

			<!-- 2. The repositories -->
			<li class="flex flex-col gap-5 p-5">
				{@render stepHead(2, t('github.s2.title'), t('github.s2.lead'), FolderGit2)}
				{#if gh.connected}
					<div class="flex flex-col gap-3 sm:pl-14">
						{#if gh.error}
							<p class="text-sm text-danger">{say(gh.error)}</p>
						{:else if installed}
							{#each gh.installations ?? [] as i (i.id)}
								<p class="text-sm">{i.account} <span class="text-muted">· {i.all ? t('github.allRepos') : t('github.someRepos')}</span></p>
							{/each}
						{/if}
						<a href={gh.install_url} class="{link} {installed ? 'bg-selected hover:bg-line' : 'bg-accent text-accent-fg hover:opacity-90'}"
							>{installed ? t('github.changeRepos') : t('github.choose')}<ExternalLink size={16} /></a
						>
					</div>
				{/if}
			</li>

			<!-- 3. Deploy -->
			<li class="flex flex-col gap-5 p-5">
				{@render stepHead(3, t('github.s3.title'), t('github.s3.lead'), Rocket)}
				{#if step === 3}
					<div class="flex flex-col gap-3 sm:pl-14">
						<a href="/new?source=github" class="{link} bg-accent text-accent-fg hover:opacity-90">{t('github.newApp')}</a>
						{#if gh.private}
							<p class="text-sm text-muted">{t('github.hookPrivate')}</p>
						{:else if gh.webhook?.state === 'ok'}
							<p class="text-sm text-ok">{t('github.hookOk', { when: ago(gh.webhook.last!) })}</p>
						{:else if gh.webhook?.state === 'failing'}
							<p class="text-sm text-danger">{t('github.hookFailing', { status: gh.webhook.status ?? '' })}</p>
						{:else}
							<p class="text-sm text-muted">{t('github.hookNone')}</p>
						{/if}
					</div>
				{/if}
			</li>
		</ol>

		{#if gh.connected}
			<section class="flex flex-col gap-3">
				<p class="text-sm text-muted">{t('github.disconnectLead')}</p>
				<ErrorText message={error} />
				<Button kind="secondary" class="self-start hover:!text-danger" {busy} onclick={disconnect}>{t('github.disconnect')}</Button>
			</section>
		{/if}
	{:else}
		<ErrorText message={error} />
	{/if}
</div>
