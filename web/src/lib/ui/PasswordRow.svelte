<script lang="ts">
	import { Check, Copy, Eye, EyeOff } from '@lucide/svelte';
	import { api } from '$lib/api';
	import { sensitive } from '$lib/confirm.svelte';
	import { messageOf } from '$lib/errors';
	import { t } from '$lib/i18n';
	import { session } from '$lib/session.svelte';
	import Button from './Button.svelte';
	import ErrorText from './ErrorText.svelte';

	// A database's password, kept out of sight until an administrator asks
	// for it. Nothing is kept once it is hidden or the page changes.
	let { db }: { db: string } = $props();
	let password = $state('');
	let busy = $state(false);
	let error = $state('');
	let copied = $state(false);
	const admin = $derived(!!session.me?.admin);

	$effect(() => {
		db;
		password = '';
		error = '';
	});

	async function show() {
		busy = true;
		error = '';
		try {
			password = (await sensitive(() => api<{ password: string }>('POST', `/apps/${encodeURIComponent(db)}/password`))).password;
		} catch (err) {
			error = messageOf(err);
		} finally {
			busy = false;
		}
	}

	async function copy() {
		try {
			await navigator.clipboard.writeText(password);
			copied = true;
			setTimeout(() => (copied = false), 2000);
		} catch {
			// It is on screen to select by hand.
		}
	}
</script>

<div class="flex flex-col gap-2">
	<div class="flex flex-wrap items-center gap-x-4 gap-y-2 rounded-2xl border border-line bg-bg px-4 py-3">
		<span class="text-sm text-muted">{t('db.passwordLabel')}</span>
		<span class="min-w-0 flex-1 basis-64 font-mono text-[15px] break-all">{password || '••••••••••••'}</span>
		{#if password}
			<Button kind="quiet" class="!h-8 shrink-0 !px-2" title={t('db.copy')} aria-label={t('db.copy')} onclick={copy}>
				{#if copied}<Check size={16} />{:else}<Copy size={16} />{/if}
			</Button>
			<Button kind="secondary" class="!h-8 !px-3.5 text-sm" onclick={() => (password = '')}><EyeOff size={16} />{t('db.passwordHide')}</Button>
		{:else if admin}
			<Button kind="secondary" class="!h-8 !px-3.5 text-sm" {busy} onclick={show}><Eye size={16} />{t('db.passwordShow')}</Button>
		{/if}
	</div>
	<p class="text-sm text-muted">{admin ? t('db.passwordShowNote') : t('db.passwordAdminOnly')}</p>
	<ErrorText message={error} />
</div>
