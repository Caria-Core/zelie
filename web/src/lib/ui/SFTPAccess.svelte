<script lang="ts">
	import { ask } from '$lib/ask.svelte';
	import { Check, Copy, TriangleAlert } from '@lucide/svelte';
	import { api } from '$lib/api';
	import { sensitive } from '$lib/confirm.svelte';
	import { messageOf } from '$lib/errors';
	import { t } from '$lib/i18n';
	import Button from './Button.svelte';
	import ErrorText from './ErrorText.svelte';

	// How to reach a game server's files over SFTP. The password belongs to
	// the server, not to a person, and is shown once, right after it is made.
	type Info = { host: string; port: number; user: string; host_key: string; running: boolean; password_set: boolean };

	let { id }: { id: string } = $props();
	let x = $state<Info | null>(null);
	let password = $state('');
	let error = $state('');
	let busy = $state(false);
	let copied = $state('');

	const path = $derived(`/games/${encodeURIComponent(id)}/sftp`);

	$effect(() => {
		password = '';
		api<Info>('GET', path).then(
			(r) => (x = r),
			(err) => (error = messageOf(err))
		);
	});

	// A name with a colon in it is an IPv6 address, which a URL brackets.
	const url = $derived(x ? `sftp://${x.user}@${x.host.includes(':') ? `[${x.host}]` : x.host}:${x.port}` : '');

	async function act(fn: () => Promise<void>) {
		busy = true;
		error = '';
		try {
			await fn();
		} catch (err) {
			error = messageOf(err);
		} finally {
			busy = false;
		}
	}

	const generate = () =>
		act(async () => {
			const r = await sensitive(() => api<{ password: string }>('POST', `${path}/password`));
			password = r.password;
			if (x) x.password_set = true;
		});

	async function reset() {
		if (!(await ask({ title: t('sftp.resetConfirm'), text: t('sftp.resetConfirmText'), action: t('sftp.reset') }))) return;
		generate();
	}

	async function removePassword() {
		if (!(await ask({ title: t('sftp.removeConfirm'), text: t('sftp.removeConfirmText'), action: t('common.remove'), danger: true }))) return;
		act(async () => {
			await sensitive(() => api('DELETE', `${path}/password`));
			password = '';
			if (x) x.password_set = false;
		});
	}

	async function copy(what: string, text: string) {
		await navigator.clipboard.writeText(text);
		copied = what;
		setTimeout(() => copied === what && (copied = ''), 2000);
	}
</script>

{#snippet copyButton(what: string, text: string)}
	<Button kind="quiet" class="!h-8 shrink-0 !px-2" title={t('sftp.copy')} aria-label={t('sftp.copy')} onclick={() => copy(what, text)}>
		{#if copied === what}<Check size={16} />{:else}<Copy size={16} />{/if}
	</Button>
{/snippet}

<section class="flex flex-col gap-4">
	<div>
		<h2 class="font-medium">{t('sftp.title')}</h2>
		<p class="text-sm text-muted">{t('sftp.lead')}</p>
	</div>
	{#if x}
		{#if !x.running}<p class="text-sm text-danger">{t('sftp.notRunning')}</p>{/if}

		<div class="flex items-center gap-2 rounded-xl bg-panel px-3 py-2">
			<code class="min-w-0 flex-1 font-mono text-[15px] break-all select-all">{url}</code>
			{@render copyButton('url', url)}
		</div>
		<p class="text-sm text-muted">{t('sftp.login', { user: x.user })}</p>

		<div class="flex flex-col gap-2">
			<h3 class="text-sm font-medium">{t('sftp.passwordTitle')}</h3>
			{#if password}
				<div class="flex flex-col gap-3 rounded-2xl border border-line bg-panel p-4">
					<p class="flex gap-2.5 text-sm"><TriangleAlert size={16} strokeWidth={1.75} class="mt-0.5 shrink-0" />{t('sftp.passwordOnce')}</p>
					<div class="flex items-center gap-2 rounded-xl bg-bg px-3 py-2">
						<code class="min-w-0 flex-1 font-mono text-[15px] break-all select-all">{password}</code>
						{@render copyButton('password', password)}
					</div>
				</div>
			{:else}
				<p class="text-sm text-muted">{x.password_set ? t('sftp.passwordSet') : t('sftp.passwordNone')}</p>
			{/if}
			<div class="flex flex-wrap gap-3">
				{#if x.password_set}
					<Button kind="secondary" {busy} onclick={reset}>{t('sftp.reset')}</Button>
					<Button kind="quiet" class="hover:!text-danger" {busy} onclick={removePassword}>{t('sftp.removePassword')}</Button>
				{:else}
					<Button kind="secondary" {busy} onclick={generate}>{t('sftp.generate')}</Button>
				{/if}
			</div>
		</div>

		<p class="text-sm text-muted">
			{t('sftp.keys')} <a href="/account" class="underline hover:text-fg">{t('sftp.keysLink')}</a>
		</p>

		{#if x.host_key}
			<div class="flex flex-col gap-2">
				<h3 class="text-sm font-medium">{t('sftp.hostKey')}</h3>
				<div class="flex items-center gap-2 rounded-xl bg-panel px-3 py-2">
					<code class="min-w-0 flex-1 font-mono text-sm break-all select-all">{x.host_key}</code>
					{@render copyButton('hostkey', x.host_key)}
				</div>
				<p class="text-sm text-muted">{t('sftp.hostKeyHint')}</p>
			</div>
		{/if}
	{/if}
	<ErrorText message={error} />
</section>
