<script lang="ts">
	import { ask } from '$lib/ask.svelte';
	import { onMount } from 'svelte';
	import { api } from '$lib/api';
	import { sensitive } from '$lib/confirm.svelte';
	import { messageOf } from '$lib/errors';
	import { date } from '$lib/format';
	import { t } from '$lib/i18n';
	import Button from './Button.svelte';
	import ErrorText from './ErrorText.svelte';
	import Field from './Field.svelte';

	// Public keys of the account. A key lets its owner log in to the game
	// servers over SFTP; the private half never leaves their machine.
	type Key = { id: number; name: string; type: string; fingerprint: string; created_at: string };

	let keys = $state<Key[] | null>(null);
	let adding = $state(false);
	let name = $state('');
	let key = $state('');
	let error = $state('');
	let busy = $state(false);

	onMount(load);

	async function load() {
		try {
			keys = await api<Key[]>('GET', '/account/ssh-keys');
		} catch (err) {
			error = messageOf(err);
		}
	}

	function open(on: boolean) {
		adding = on;
		name = key = error = '';
	}

	async function add(e: SubmitEvent) {
		e.preventDefault();
		busy = true;
		error = '';
		try {
			await sensitive(() => api('POST', '/account/ssh-keys', { name: name.trim(), key: key.trim() }));
			open(false);
			await load();
		} catch (err) {
			error = messageOf(err);
		} finally {
			busy = false;
		}
	}

	async function remove(k: Key) {
		if (!(await ask({ title: t('sshkeys.removeConfirm', { name: k.name }), text: t('sshkeys.removeConfirmText'), action: t('common.remove'), danger: true }))) return;
		busy = true;
		error = '';
		try {
			await sensitive(() => api('DELETE', `/account/ssh-keys/${k.id}`));
			await load();
		} catch (err) {
			error = messageOf(err);
		} finally {
			busy = false;
		}
	}
</script>

<section class="flex flex-col gap-4 border-t border-line pt-8">
	<div>
		<h2 class="font-medium">{t('sshkeys.title')}</h2>
		<p class="text-sm text-muted">{t('sshkeys.lead')}</p>
	</div>
	{#if keys?.length}
		<ul class="flex flex-col divide-y divide-line rounded-2xl border border-line">
			{#each keys as k (k.id)}
				<li class="flex flex-wrap items-center justify-between gap-x-4 gap-y-2 px-4 py-3">
					<div class="min-w-0">
						<p class="truncate">{k.name}</p>
						<p class="text-sm break-all text-muted"><span class="font-mono">{k.fingerprint}</span></p>
						<p class="text-sm text-muted">{k.type} · {t('account.added', { date: date(k.created_at) })}</p>
					</div>
					<Button kind="quiet" class="!h-auto !px-0 text-sm hover:!text-danger" disabled={busy} onclick={() => remove(k)}>{t('account.remove')}</Button>
				</li>
			{/each}
		</ul>
	{/if}
	{#if adding}
		<form class="flex max-w-xl flex-col gap-4" onsubmit={add}>
			<Field label={t('sshkeys.name')} hint={t('sshkeys.nameHint')} maxlength={64} autocomplete="off" bind:value={name} />
			<div class="flex flex-col gap-1.5">
				<label for="ssh-key" class="text-sm font-medium">{t('sshkeys.key')}</label>
				<textarea
					id="ssh-key"
					rows="4"
					required
					autocomplete="off"
					autocapitalize="off"
					spellcheck="false"
					placeholder="ssh-ed25519 AAAA…"
					bind:value={key}
					class="w-full min-w-0 rounded-xl border border-line bg-bg px-3.5 py-2.5 font-mono text-sm outline-none transition focus:border-muted"
				></textarea>
				<p class="text-sm text-muted">{t('sshkeys.keyHint')}</p>
			</div>
			<ErrorText message={error} />
			<div class="flex gap-3">
				<Button type="submit" {busy}>{t('sshkeys.add')}</Button>
				<Button kind="secondary" type="button" onclick={() => open(false)}>{t('common.cancel')}</Button>
			</div>
		</form>
	{:else}
		<ErrorText message={error} />
		{#if keys}<Button kind="secondary" class="self-start" onclick={() => open(true)}>{t('sshkeys.addKey')}</Button>{/if}
	{/if}
</section>
