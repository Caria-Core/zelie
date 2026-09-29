<script lang="ts">
	import { ask } from '$lib/ask.svelte';
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { api } from '$lib/api';
	import { sensitive } from '$lib/confirm.svelte';
	import { messageOf } from '$lib/errors';
	import { ago, date, device } from '$lib/format';
	import { t } from '$lib/i18n';
	import { addPasskey, onDomain, passkeysAvailable } from '$lib/passkey';
	import { refresh } from '$lib/session.svelte';
	import Button from '$lib/ui/Button.svelte';
	import ErrorText from '$lib/ui/ErrorText.svelte';
	import Field from '$lib/ui/Field.svelte';
	import Lead from '$lib/ui/Lead.svelte';
	import RecoveryCodes from '$lib/ui/RecoveryCodes.svelte';
	import SSHKeys from '$lib/ui/SSHKeys.svelte';

	type Account = {
		email: string;
		totp: boolean;
		recovery_left: number;
		passkeys: { id: string; name: string; created_at: string; used_at?: string }[];
		sessions: { id: string; current: boolean; verified: boolean; ip: string; agent: string; seen_at: string }[];
	};

	let acct = $state<Account | null>(null);
	// One form or flow is open at a time; busy and error belong to it.
	let open = $state<'' | 'password' | 'passkey' | 'totp' | 'codes'>('');
	let busy = $state(false);
	let error = $state('');

	let current = $state('');
	let next = $state('');
	let keyName = $state('');
	let totp = $state<{ secret: string; qr: string } | null>(null);
	let code = $state('');
	let codes = $state<string[]>([]);
	let saved = $state('');

	const canPasskey = passkeysAvailable() && onDomain();
	const factors = $derived((acct?.passkeys.length ?? 0) + (acct?.totp ? 1 : 0));

	onMount(load);

	async function load() {
		const a = await api<Account>('GET', '/account');
		// This browser first, the rest by when they were last used.
		a.sessions.sort((x, y) => Number(y.current) - Number(x.current));
		acct = a;
	}

	function show(what: typeof open) {
		open = what;
		error = saved = '';
		current = next = code = '';
		totp = null;
	}

	async function run(fn: () => Promise<unknown>, done = '') {
		busy = true;
		error = '';
		try {
			await fn();
			await Promise.all([load(), refresh()]);
			if (done) show('');
			saved = done;
		} catch (err) {
			error = messageOf(err);
		} finally {
			busy = false;
		}
	}

	const changePassword = (e: SubmitEvent) => {
		e.preventDefault();
		run(() => sensitive(() => api('POST', '/account/password', { current, new: next })), t('account.password.done'));
	};

	const newPasskey = (e: SubmitEvent) => {
		e.preventDefault();
		run(() => sensitive(() => addPasskey(keyName.trim() || t('account.passkeys.default'))), t('account.passkeys.done'));
	};

	async function removePasskey(id: string, name: string) {
		if (!(await ask({ title: t('account.passkeys.removeConfirm', { name }), text: t('account.passkeys.removeConfirmText'), action: t('common.remove'), danger: true }))) return;
		show('');
		run(() => sensitive(() => api('DELETE', `/2fa/passkey/${id}`)));
	}

	const startTotp = () =>
		run(async () => {
			const out = await sensitive(() => api<{ secret: string; qr: string }>('POST', '/2fa/totp/new'));
			show('totp');
			totp = out;
		});

	const confirmTotp = (e: SubmitEvent) => {
		e.preventDefault();
		run(() => sensitive(() => api('POST', '/2fa/totp', { code: code.trim() })), t('account.totp.done'));
	};

	async function removeTotp() {
		if (!(await ask({ title: t('account.totp.removeConfirm'), text: t('account.totp.removeConfirmText'), action: t('common.turnOff'), danger: true }))) return;
		show('');
		run(() => sensitive(() => api('DELETE', '/2fa/totp')));
	}

	async function newCodes() {
		if (!(await ask({ title: t('account.recovery.confirm'), text: t('account.recovery.confirmText'), action: t('account.recovery.new') }))) return;
		run(async () => {
			const out = await sensitive(() => api<{ recovery_codes: string[] }>('POST', '/2fa/recovery'));
			show('codes');
			codes = out.recovery_codes;
		});
	}

	const endSession = (id: string, current: boolean) =>
		run(async () => {
			await api('DELETE', `/sessions/${id}`);
			if (current) await goto('/login');
		});

	const endOthers = () => run(() => api('POST', '/sessions/end-others'));

	const section = 'flex flex-col gap-4 border-t border-line pt-8';
	const row = 'flex flex-wrap items-center justify-between gap-x-4 gap-y-2 px-4 py-3';
	const list = 'flex flex-col divide-y divide-line rounded-2xl border border-line';
</script>

<div class="flex max-w-2xl flex-col gap-8">
	<Lead title={t('account.title')} text={' ' + t('account.lead')} />
	{#if saved}<p role="status" class="text-sm text-ok">{saved}</p>{/if}
	{#if !open}<ErrorText message={error} />{/if}

	{#if acct}
		<section class={section}>
			<div class="flex flex-wrap items-baseline justify-between gap-2">
				<div>
					<h2 class="font-medium">{t('account.password.title')}</h2>
					<p class="text-sm text-muted">{acct.email}</p>
				</div>
				{#if open !== 'password'}
					<Button kind="secondary" onclick={() => show('password')}>{t('account.password.change')}</Button>
				{/if}
			</div>
			{#if open === 'password'}
				<form class="flex max-w-sm flex-col gap-4" onsubmit={changePassword}>
					<!-- A hidden username lets password managers file the new password under the right account. -->
					<input type="email" autocomplete="username" value={acct.email} hidden readonly />
					<Field label={t('account.password.current')} type="password" autocomplete="current-password" required bind:value={current} />
					<Field
						label={t('account.password.new')}
						hint={t('account.password.hint')}
						type="password"
						autocomplete="new-password"
						minlength={10}
						required
						bind:value={next}
					/>
					<ErrorText message={error} />
					<div class="flex gap-3">
						<Button type="submit" {busy}>{t('account.password.save')}</Button>
						<Button kind="secondary" type="button" onclick={() => show('')}>{t('common.cancel')}</Button>
					</div>
				</form>
			{/if}
		</section>

		<section class={section}>
			<div>
				<h2 class="font-medium">{t('account.passkeys.title')}</h2>
				<p class="text-sm text-muted">{canPasskey ? t('enroll.passkeyLead') : t('enroll.passkeyNoDomain')}</p>
			</div>
			{#if acct.passkeys.length}
				<ul class={list}>
					{#each acct.passkeys as k (k.id)}
						<li class={row}>
							<div class="min-w-0">
								<p class="truncate">{k.name}</p>
								<p class="text-sm text-muted">
									{t('account.added', { date: date(k.created_at) })}{#if k.used_at}
										· {t('account.used', { when: ago(k.used_at) })}{/if}
								</p>
							</div>
							<Button
								kind="quiet"
								class="!h-auto !px-0 text-sm hover:!text-danger"
								disabled={busy || factors < 2}
								title={factors < 2 ? t('account.lastFactor') : undefined}
								onclick={() => removePasskey(k.id, k.name)}>{t('account.remove')}</Button
							>
						</li>
					{/each}
				</ul>
			{/if}
			{#if open === 'passkey'}
				<form class="flex max-w-sm flex-col gap-4" onsubmit={newPasskey}>
					<Field label={t('account.passkeys.name')} hint={t('account.passkeys.nameHint')} maxlength={60} bind:value={keyName} />
					<ErrorText message={error} />
					<div class="flex gap-3">
						<Button type="submit" {busy}>{t('common.continue')}</Button>
						<Button kind="secondary" type="button" onclick={() => show('')}>{t('common.cancel')}</Button>
					</div>
				</form>
			{:else if canPasskey}
				<Button kind="secondary" class="self-start" onclick={() => show('passkey')}>{t('enroll.passkey')}</Button>
			{/if}
		</section>

		<section class={section}>
			<div class="flex flex-wrap items-baseline justify-between gap-2">
				<div>
					<h2 class="font-medium">{t('account.totp.title')}</h2>
					<p class="text-sm text-muted">{acct.totp ? t('account.totp.on') : t('account.totp.off')}</p>
				</div>
				{#if open !== 'totp'}
					<div class="flex gap-4">
						{#if acct.totp}
							<Button
								kind="quiet"
								class="!h-auto !px-0 text-sm hover:!text-danger"
								disabled={busy || factors < 2}
								title={factors < 2 ? t('account.lastFactor') : undefined}
								onclick={removeTotp}>{t('account.remove')}</Button
							>
						{/if}
						<Button kind="secondary" {busy} onclick={startTotp}>{acct.totp ? t('account.totp.replace') : t('account.totp.add')}</Button>
					</div>
				{/if}
			</div>
			{#if open === 'totp' && totp}
				<p class="text-sm text-muted">{t('enroll.scan')} {t('enroll.scanMore')}</p>
				<div class="flex flex-col items-start gap-3">
					<img src={totp.qr} alt="" class="size-44 rounded-xl border border-line" />
					<code class="font-mono text-sm break-all text-muted select-all">{totp.secret}</code>
				</div>
				<form class="flex max-w-sm flex-col gap-4" onsubmit={confirmTotp}>
					<Field
						label={t('enroll.code')}
						inputmode="numeric"
						autocomplete="one-time-code"
						pattern="[0-9]{'{6}'}"
						maxlength={6}
						required
						bind:value={code}
					/>
					<ErrorText message={error} />
					<div class="flex gap-3">
						<Button type="submit" {busy}>{t('common.continue')}</Button>
						<Button kind="secondary" type="button" onclick={() => show('')}>{t('common.cancel')}</Button>
					</div>
				</form>
			{/if}
		</section>

		<section class={section}>
			<div class="flex flex-wrap items-baseline justify-between gap-2">
				<div>
					<h2 class="font-medium">{t('account.recovery.title')}</h2>
					<p class="text-sm {acct.recovery_left < 3 ? 'text-danger' : 'text-muted'}">
						{t('account.recovery.left', { n: acct.recovery_left })}
					</p>
				</div>
				{#if open !== 'codes'}
					<Button kind="secondary" {busy} onclick={newCodes}>{t('account.recovery.new')}</Button>
				{/if}
			</div>
			{#if open === 'codes'}
				<p class="text-sm text-muted">{t('enroll.recovery.lead')}</p>
				<RecoveryCodes {codes}>
					<Button onclick={() => show('')}>{t('enroll.recovery.saved')}</Button>
				</RecoveryCodes>
			{/if}
		</section>

		<SSHKeys />

		<section class={section}>
			<div class="flex flex-wrap items-baseline justify-between gap-2">
				<div>
					<h2 class="font-medium">{t('account.sessions.title')}</h2>
					<p class="text-sm text-muted">{t('account.sessions.lead')}</p>
				</div>
				{#if acct.sessions.length > 1}
					<Button kind="secondary" {busy} onclick={endOthers}>{t('account.sessions.endOthers')}</Button>
				{/if}
			</div>
			<ul class={list}>
				{#each acct.sessions as x (x.id)}
					<li class={row}>
						<div class="min-w-0">
							<p class="truncate">
								{device(x.agent)}
								{#if x.current}<span class="ml-1.5 rounded-full bg-selected px-2 py-0.5 text-xs">{t('account.sessions.this')}</span>{/if}
							</p>
							<p class="text-sm text-muted">
								<span class="font-mono">{x.ip}</span> · {x.current ? t('account.sessions.now') : ago(x.seen_at)}
							</p>
							{#if !x.verified}<p class="text-sm text-danger">{t('account.sessions.half')}</p>{/if}
						</div>
						<Button kind="quiet" class="!h-auto !px-0 text-sm" disabled={busy} onclick={() => endSession(x.id, x.current)}
							>{t('nav.logout')}</Button
						>
					</li>
				{/each}
			</ul>
		</section>
	{/if}
</div>
