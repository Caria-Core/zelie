<script lang="ts">
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { api } from '$lib/api';
	import { messageOf } from '$lib/errors';
	import { t } from '$lib/i18n';
	import { addPasskey, onDomain, passkeysAvailable } from '$lib/passkey';
	import { refresh } from '$lib/session.svelte';
	import Button from '$lib/ui/Button.svelte';
	import ErrorText from '$lib/ui/ErrorText.svelte';
	import Field from '$lib/ui/Field.svelte';
	import Lead from '$lib/ui/Lead.svelte';

	type Totp = { secret: string; qr: string };

	let step = $state<'loading' | 'choose' | 'totp' | 'codes'>('loading');
	let totp = $state<Totp | null>(null);
	let code = $state('');
	let codes = $state<string[]>([]);
	let copied = $state(false);
	let error = $state('');
	let busy = $state(false);
	const passkeys = passkeysAvailable() && onDomain();

	onMount(async () => {
		const me = await refresh();
		if (!me.logged_in) return goto('/login');
		if (!me.enroll) return goto(me.verified ? '/' : '/login');
		step = 'choose';
	});

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

	function finished(res: { recovery_codes?: string[] }) {
		if (res.recovery_codes?.length) {
			codes = res.recovery_codes;
			step = 'codes';
		} else goto('/');
	}

	const startPasskey = () => run(async () => finished(await addPasskey(navigator.platform || 'Passkey')));

	const startTotp = () =>
		run(async () => {
			totp = await api<Totp>('POST', '/2fa/totp/new');
			step = 'totp';
		});

	const confirmTotp = (e: SubmitEvent) => {
		e.preventDefault();
		run(async () => finished(await api('POST', '/2fa/totp', { code: code.trim() })));
	};

	async function copy() {
		await navigator.clipboard.writeText(codes.join('\n'));
		copied = true;
	}
</script>

{#if step === 'choose'}
	<Lead title={t('enroll.title')} text={' ' + t('enroll.lead')} />
	<div class="flex flex-col gap-3">
		<button
			class="rounded-2xl border border-line p-4 text-left transition hover:bg-hover disabled:opacity-50"
			disabled={!passkeys || busy}
			onclick={startPasskey}
		>
			<p class="font-medium">{t('enroll.passkey')}</p>
			<p class="text-sm text-muted">{passkeys ? t('enroll.passkeyLead') : t('enroll.passkeyNoDomain')}</p>
		</button>
		<button
			class="rounded-2xl border border-line p-4 text-left transition hover:bg-hover disabled:opacity-50"
			disabled={busy}
			onclick={startTotp}
		>
			<p class="font-medium">{t('enroll.totp')}</p>
			<p class="text-sm text-muted">{t('enroll.totpLead')}</p>
		</button>
	</div>
	<ErrorText message={error} />
{:else if step === 'totp' && totp}
	<Lead title={t('enroll.scan')} text={' ' + t('enroll.scanMore')} />
	<div class="flex flex-col items-start gap-3">
		<img src={totp.qr} alt="" class="size-44 rounded-xl border border-line" />
		<code class="font-mono text-sm break-all text-muted select-all">{totp.secret}</code>
	</div>
	<form class="flex flex-col gap-4" onsubmit={confirmTotp}>
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
			<Button kind="secondary" type="button" onclick={() => ((step = 'choose'), (error = ''))}>{t('common.cancel')}</Button>
		</div>
	</form>
{:else if step === 'codes'}
	<Lead title={t('enroll.recovery.title')} text={' ' + t('enroll.recovery.lead')} />
	<ul class="grid grid-cols-2 gap-x-6 gap-y-1.5 rounded-2xl bg-panel p-5 font-mono text-[15px]">
		{#each codes as c (c)}<li>{c}</li>{/each}
	</ul>
	<div class="flex gap-3">
		<Button onclick={() => goto('/')}>{t('enroll.recovery.saved')}</Button>
		<Button kind="secondary" onclick={copy}>{copied ? t('enroll.recovery.copied') : t('enroll.recovery.copy')}</Button>
	</div>
{/if}
