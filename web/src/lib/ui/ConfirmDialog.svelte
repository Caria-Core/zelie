<script lang="ts">
	import { api } from '$lib/api';
	import { confirming } from '$lib/confirm.svelte';
	import { messageOf } from '$lib/errors';
	import { t } from '$lib/i18n';
	import { passkeysAvailable, usePasskey } from '$lib/passkey';
	import { session } from '$lib/session.svelte';
	import Button from './Button.svelte';
	import ErrorText from './ErrorText.svelte';
	import Field from './Field.svelte';
	import Lead from './Lead.svelte';

	let dialog = $state<HTMLDialogElement>();
	let recovery = $state(false);
	let code = $state('');
	let error = $state('');
	let busy = $state(false);
	const methods = $derived(session.me?.methods ?? []);
	const passkey = $derived(methods.includes('passkey') && passkeysAvailable());

	$effect(() => {
		if (confirming.answer && !dialog?.open) {
			recovery = false;
			code = error = '';
			dialog?.showModal();
		}
	});

	function done(ok: boolean) {
		dialog?.close();
		confirming.answer?.(ok);
	}

	async function run(fn: () => Promise<unknown>) {
		busy = true;
		error = '';
		try {
			await fn();
			done(true);
		} catch (err) {
			error = messageOf(err);
		} finally {
			busy = false;
		}
	}

	const submit = (e: SubmitEvent) => {
		e.preventDefault();
		run(() => api('POST', '/confirm', recovery ? { recovery: code.trim() } : { totp: code.trim() }));
	};
</script>

<!-- Escape closes the dialog through the cancel event, which counts as no. -->
<dialog
	bind:this={dialog}
	oncancel={(e) => (e.preventDefault(), done(false))}
	class="m-auto w-[min(26rem,calc(100vw-2rem))] rounded-2xl border border-line bg-bg p-6 text-fg backdrop:bg-black/40"
>
	<div class="flex flex-col gap-5">
		<Lead title={t('confirm.title')} text={' ' + t('confirm.lead')} />
		{#if passkey && !recovery}
			<Button onclick={() => run(() => usePasskey('/confirm/passkey'))} {busy} class="self-start">{t('confirm.passkey')}</Button>
		{/if}
		{#if recovery || methods.includes('totp')}
			<form class="flex flex-col gap-4" onsubmit={submit}>
				{#if recovery}
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
				<Button type="submit" {busy} kind={passkey && !recovery ? 'secondary' : 'primary'} class="self-start"
					>{t('common.continue')}</Button
				>
			</form>
		{:else}
			<ErrorText message={error} />
		{/if}
		<div class="flex gap-4 text-sm">
			<Button kind="quiet" onclick={() => done(false)} class="!h-auto !px-0">{t('common.cancel')}</Button>
			{#if !recovery}
				<Button kind="quiet" onclick={() => ((recovery = true), (code = error = ''))} class="!h-auto !px-0"
					>{t('login.second.recovery')}</Button
				>
			{/if}
		</div>
	</div>
</dialog>
