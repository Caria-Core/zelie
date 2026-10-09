<script lang="ts">
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { api, ApiError, type Me } from '$lib/api';
	import { solvePow } from '$lib/pow';
	import { messageOf } from '$lib/errors';
	import { t } from '$lib/i18n';
	import { passkeysAvailable, usePasskey } from '$lib/passkey';
	import { refresh } from '$lib/session.svelte';
	import Button from '$lib/ui/Button.svelte';
	import ErrorText from '$lib/ui/ErrorText.svelte';
	import Field from '$lib/ui/Field.svelte';
	import Lead from '$lib/ui/Lead.svelte';
	import LoadError from '$lib/ui/LoadError.svelte';

	let step = $state<'loading' | 'password' | 'second' | 'recovery'>('loading');
	let me = $state<Me | null>(null);
	let email = $state('');
	let password = $state('');
	let code = $state('');
	let error = $state('');
	let busy = $state(false);
	// Set while the browser works on the panel's puzzle.
	let checking = $state(false);
	const showPasskey = $derived(!!me?.methods?.includes('passkey') && passkeysAvailable());

	onMount(start);

	async function start() {
		error = '';
		try {
			const { open } = await api<{ open: boolean }>('GET', '/setup');
			if (open) return goto('/setup');
			route(await refresh());
		} catch (err) {
			error = messageOf(err);
		}
	}

	function route(m: Me) {
		me = m;
		if (!m.logged_in) step = 'password';
		else if (m.verified) goto('/');
		else if (m.enroll) goto('/enroll');
		else step = 'second';
	}

	async function run(fn: () => Promise<unknown>) {
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

	const submitPassword = (e: SubmitEvent) => {
		e.preventDefault();
		run(async () => {
			let pow: { challenge: string; nonce: string } | undefined;
			// After a few wrong passwords from here or for this account the panel
			// wants a puzzle solved, then the same request again. Other guesses
			// can make it bigger meanwhile, so it may ask again.
			for (let round = 0; ; round++) {
				try {
					route(await api<Me>('POST', '/login', { email, password, pow }));
					return;
				} catch (err) {
					if (!(err instanceof ApiError && err.msg.code === 'login.pow') || round === 2) throw err;
					checking = true;
					try {
						const p = err.msg.params ?? {};
						pow = await solvePow(String(p.challenge), Number(p.bits));
					} finally {
						checking = false;
					}
				}
			}
		});
	};

	const submitCode = (e: SubmitEvent) => {
		e.preventDefault();
		run(async () => {
			await api('POST', step === 'recovery' ? '/login/recovery' : '/login/totp', { code: code.trim() });
			await goto('/');
		});
	};

	const passkey = () =>
		run(async () => {
			await usePasskey('/login/passkey');
			await goto('/');
		});

	async function back() {
		await api('POST', '/logout');
		code = password = error = '';
		step = 'password';
	}
</script>

{#if step === 'loading'}
	{#if error}<LoadError message={error} retry={start} />{/if}
{:else if step === 'password'}
	<Lead title={t('login.title')} text={' ' + t('login.lead')} />
	<form class="flex flex-col gap-4" onsubmit={submitPassword}>
		<Field label={t('common.email')} type="email" autocomplete="username" required bind:value={email} />
		<Field label={t('common.password')} type="password" autocomplete="current-password" required bind:value={password} />
		<ErrorText message={error} />
		{#if checking}<p role="status" class="text-sm text-muted">{t('login.checking')}</p>{/if}
		<Button type="submit" {busy} class="mt-2 self-start">{t('login.submit')}</Button>
	</form>
{:else if step === 'second' || step === 'recovery'}
	<Lead title={t('login.second.title')} text={me?.email ? ' ' + me.email : ''} />
	{#if step === 'second' && showPasskey}
		<Button onclick={passkey} {busy} class="self-start">{t('login.second.passkey')}</Button>
	{/if}
	{#if step === 'recovery' || me?.methods?.includes('totp')}
		<form class="flex flex-col gap-4" onsubmit={submitCode}>
			{#if step === 'recovery'}
				<Field label={t('login.second.recoveryLabel')} autocomplete="off" required bind:value={code} />
			{:else}
				<Field
					label={t('login.second.code')}
					inputmode="numeric"
					autocomplete="one-time-code"
					pattern="[0-9]{'{6}'}"
					maxlength={6}
					required
					bind:value={code}
				/>
			{/if}
			<ErrorText message={error} />
			<Button type="submit" {busy} kind={step === 'second' && showPasskey ? 'secondary' : 'primary'} class="self-start"
				>{t('common.continue')}</Button
			>
		</form>
	{:else}
		<ErrorText message={error} />
	{/if}
	<div class="flex gap-4 text-sm">
		<Button kind="quiet" onclick={back} class="!h-auto !px-0">{t('login.second.back')}</Button>
		{#if step === 'second'}
			<Button kind="quiet" onclick={() => ((step = 'recovery'), (code = error = ''))} class="!h-auto !px-0"
				>{t('login.second.recovery')}</Button
			>
		{/if}
	</div>
{/if}
