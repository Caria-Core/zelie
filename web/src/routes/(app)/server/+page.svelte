<script lang="ts">
	import { ask } from '$lib/ask.svelte';
	import { onMount } from 'svelte';
	import { ArrowUpRight, CircleCheck, RefreshCw, TriangleAlert } from '@lucide/svelte';
	import { api } from '$lib/api';
	import { bytes } from '$lib/backups';
	import { sensitive } from '$lib/confirm.svelte';
	import { messageOf } from '$lib/errors';
	import { ago, date } from '$lib/format';
	import { t } from '$lib/i18n';
	import { loadServer, server, type NodeInfo, type ServerInfo } from '$lib/server.svelte';
	import Button from '$lib/ui/Button.svelte';
	import ErrorText from '$lib/ui/ErrorText.svelte';
	import Field from '$lib/ui/Field.svelte';
	import Lead from '$lib/ui/Lead.svelte';
	import SFTPPort from '$lib/ui/SFTPPort.svelte';

	let error = $state('');
	let busy = $state(false);
	// The version being installed while the services restart.
	let updating = $state('');

	const info = $derived(server.info);
	let node = $state<NodeInfo | null>(null);
	let override = $state('');
	let savingAddress = $state(false);
	let addressError = $state('');
	let addressSaved = $state(false);

	onMount(() => {
		loadServer().catch((err) => (error = messageOf(err)));
		api<NodeInfo>('GET', '/nodes/1')
			.then((n) => {
				node = n;
				override = n.override;
			})
			.catch((err) => (addressError = messageOf(err)));
	});

	async function saveAddress(e: SubmitEvent) {
		e.preventDefault();
		savingAddress = true;
		addressError = '';
		addressSaved = false;
		try {
			node = await api<NodeInfo>('PUT', '/nodes/1', { public_address: override.trim() });
			override = node.override;
			addressSaved = true;
		} catch (err) {
			addressError = messageOf(err);
		} finally {
			savingAddress = false;
		}
	}

	let steamKey = $state('');
	let steamBusy = $state(false);
	let steamError = $state('');
	let steamSaved = $state(false);

	async function saveSteamKey(e: SubmitEvent) {
		e.preventDefault();
		steamBusy = true;
		steamError = '';
		steamSaved = false;
		try {
			await sensitive(() => api('PUT', '/server/steam-key', { key: steamKey.trim() }));
			steamKey = '';
			steamSaved = true;
			await loadServer();
		} catch (err) {
			steamError = messageOf(err);
		} finally {
			steamBusy = false;
		}
	}

	async function removeSteamKey() {
		steamBusy = true;
		steamError = '';
		steamSaved = false;
		try {
			await sensitive(() => api('DELETE', '/server/steam-key'));
			await loadServer();
		} catch (err) {
			steamError = messageOf(err);
		} finally {
			steamBusy = false;
		}
	}

	async function check() {
		busy = true;
		error = '';
		try {
			server.info = await api<ServerInfo>('POST', '/server/check');
		} catch (err) {
			error = messageOf(err);
		} finally {
			busy = false;
		}
	}

	async function update() {
		const to = info?.available?.version;
		if (!to || !(await ask({ title: t('server.updateConfirm', { version: to }), text: t('server.updateConfirmText'), action: t('server.update', { version: to }) }))) return;
		busy = true;
		error = '';
		try {
			await sensitive(() => api('POST', '/server/update'));
			updating = to;
			await follow(to);
		} catch (err) {
			error = messageOf(err);
		} finally {
			busy = false;
			updating = '';
		}
	}

	// follow asks until the new version answers or the update is given up
	// on. While the services restart, requests fail; that is expected.
	async function follow(to: string) {
		const until = Date.now() + 4 * 60_000;
		while (Date.now() < until) {
			await new Promise((r) => setTimeout(r, 2000));
			const s = await loadServer().catch(() => null);
			if (!s) continue;
			if (s.version === to) return;
			if (s.last_update?.to === to && !s.last_update.running && !s.last_update.ok) return;
		}
		error = t('server.updateSlow');
	}

	const access = $derived(
		info?.access === 'tunnel'
			? t('server.accessTunnel')
			: info?.access === 'self-signed'
				? t('server.accessSelfSigned')
				: info?.access === 'acme'
					? t('server.accessAcme')
					: ''
	);
</script>

<div class="flex max-w-2xl flex-col gap-10">
	<Lead title={t('server.title')} text={info ? ' ' + t('server.lead', { version: info.version }) : ''} />

	{#if info}
		<section class="flex flex-col gap-4">
			<h2 class="font-medium">{t('server.version')}</h2>

			{#if updating}
				<div class="flex gap-3 rounded-2xl border border-line p-5">
					<RefreshCw size={18} strokeWidth={1.75} class="mt-0.5 shrink-0 animate-spin" />
					<div class="flex flex-col gap-1">
						<p class="font-medium">{t('server.updating', { version: updating })}</p>
						<p class="text-sm text-muted">{t('server.updatingLead')}</p>
					</div>
				</div>
			{:else if info.available}
				<div class="flex flex-col gap-4 rounded-2xl border border-line p-5">
					<div class="flex flex-wrap items-baseline justify-between gap-2">
						<p class="font-medium">{t('server.available', { version: info.available.version })}</p>
						{#if info.available.published}<p class="text-sm text-muted">{date(info.available.published)}</p>{/if}
					</div>
					{#if info.available.notes}
						<p class="max-h-60 overflow-auto rounded-xl bg-panel px-4 py-3 text-sm whitespace-pre-line text-muted">{info.available.notes.trim()}</p>
					{/if}
					<p class="text-sm text-muted">{t('server.updateLead')}</p>
					<div class="flex flex-wrap items-center gap-3">
						<Button {busy} onclick={update}>{t('server.update', { version: info.available.version })}</Button>
						{#if info.available.url}
							<a
								href={info.available.url}
								target="_blank"
								rel="noopener noreferrer"
								class="inline-flex items-center gap-1 text-sm text-muted hover:text-fg"
								>{t('server.releaseNotes')}<ArrowUpRight size={14} /></a
							>
						{/if}
					</div>
				</div>
			{:else}
				<div class="flex flex-wrap items-center justify-between gap-3 rounded-2xl border border-line p-5">
					<div class="flex gap-3">
						<CircleCheck size={18} strokeWidth={1.75} class="mt-0.5 shrink-0" />
						<div>
							<p class="font-medium">{t('server.upToDate')}</p>
							<p class="text-sm text-muted">
								{#if info.check_error}{t('server.checkFailed')}{:else if info.checked_at}{t('server.checked', {
										when: ago(info.checked_at)
									})}{/if}
							</p>
						</div>
					</div>
					<Button kind="secondary" {busy} onclick={check}>{t('server.check')}</Button>
				</div>
			{/if}

			{#if info.last_update && !updating}
				{#if !info.last_update.ok && !info.last_update.running}
					<p class="flex gap-2.5 text-sm text-danger">
						<TriangleAlert size={16} strokeWidth={1.75} class="mt-0.5 shrink-0" />
						<span
							>{t('server.lastFailed', { version: info.last_update.to, when: ago(info.last_update.at) })}
							<span class="text-muted">{info.last_update.error}</span></span
						>
					</p>
				{:else if info.last_update.ok}
					<p class="text-sm text-muted">
						{t('server.lastOk', { from: info.last_update.from, to: info.last_update.to, when: ago(info.last_update.at) })}
					</p>
				{/if}
			{/if}
			<ErrorText message={error} />
		</section>

		<section class="flex flex-col gap-4">
			<h2 class="font-medium">{t('server.machine')}</h2>
			<dl class="grid grid-cols-2 gap-px overflow-hidden rounded-2xl border border-line bg-line sm:grid-cols-3">
				<div class="flex min-w-0 flex-col gap-1 bg-bg px-4 py-3">
					<dt class="text-sm text-muted">{t('server.address')}</dt>
					<dd class="text-[15px] break-words">{info.address ?? '—'}</dd>
				</div>
				<div class="flex min-w-0 flex-col gap-1 bg-bg px-4 py-3">
					<dt class="text-sm text-muted">{t('server.access')}</dt>
					<dd class="text-[15px] break-words">{access || '—'}</dd>
				</div>
				{#if info.host}
					<div class="flex min-w-0 flex-col gap-1 bg-bg px-4 py-3">
						<dt class="text-sm text-muted">{t('server.cpus')}</dt>
						<dd class="text-[15px]">{info.host.cpus}</dd>
					</div>
					<div class="flex min-w-0 flex-col gap-1 bg-bg px-4 py-3">
						<dt class="text-sm text-muted">{t('server.memory')}</dt>
						<dd class="text-[15px]">{bytes(info.host.memory_bytes)}</dd>
					</div>
					<div class="col-span-2 flex min-w-0 flex-col gap-1 bg-bg px-4 py-3">
						<dt class="text-sm text-muted">{t('server.disk')}</dt>
						<dd class="text-[15px]">
							{t('server.diskFree', { free: bytes(info.host.disk_free_bytes), total: bytes(info.host.disk_bytes) })}
						</dd>
					</div>
				{/if}
			</dl>
		</section>

		{#if node}
			<section class="flex flex-col gap-4">
				<div>
					<h2 class="font-medium">{t('server.gameAddress')}</h2>
					<p class="text-sm text-muted">{t('server.gameAddressLead')}</p>
				</div>
				<p class="text-sm text-muted">
					{#if !node.detected}{t('server.detectedNone')}{:else if node.private}{t('server.detectedPrivate', { address: node.detected })}{:else}{t('server.detected', { address: node.detected })}{/if}
				</p>
				<form class="flex flex-col gap-4" onsubmit={saveAddress}>
					<Field
						label={t('server.addressOverride')}
						hint={t('server.addressHint')}
						placeholder="play.example.com"
						autocomplete="off"
						autocapitalize="off"
						spellcheck="false"
						bind:value={override}
					/>
					<div class="flex items-center gap-3">
						<Button type="submit" busy={savingAddress}>{t('server.saveAddress')}</Button>
						{#if addressSaved}<CircleCheck size={16} strokeWidth={1.75} class="text-muted" />{/if}
					</div>
					<ErrorText message={addressError} />
				</form>
			</section>
		{/if}
		<section class="flex flex-col gap-4">
			<div>
				<h2 class="font-medium">{t('server.steamKey')}</h2>
				<p class="text-sm text-muted">{t('server.steamKeyLead')}</p>
			</div>
			<p class="text-sm text-muted">{t('server.steamKeyAdds')}</p>
			<p class="text-sm text-muted">{t('server.steamKeyPrivacy')}</p>
			<a href="https://steamcommunity.com/dev/apikey" target="_blank" rel="noopener noreferrer" class="inline-flex w-fit items-center gap-1 text-sm text-muted hover:text-fg">{t('server.steamKeyGet')}<ArrowUpRight size={14} /></a>
			<form class="flex flex-col gap-4" onsubmit={saveSteamKey}>
				<Field
					label={info.steam_key_set ? t('server.steamKeyReplace') : t('server.steamKeyLabel')}
					hint={info.steam_key_set ? t('server.steamKeyIsSet') : ''}
					type="password"
					autocomplete="off"
					autocapitalize="off"
					spellcheck="false"
					bind:value={steamKey}
				/>
				<div class="flex flex-wrap items-center gap-3">
					<Button type="submit" busy={steamBusy} disabled={!steamKey.trim()}>{t('server.steamKeySave')}</Button>
					{#if info.steam_key_set}<Button type="button" kind="quiet" busy={steamBusy} onclick={removeSteamKey}>{t('common.remove')}</Button>{/if}
					{#if steamSaved}<CircleCheck size={16} strokeWidth={1.75} class="text-muted" />{/if}
				</div>
				<ErrorText message={steamError} />
			</form>
		</section>
		<SFTPPort />
	{:else}
		<ErrorText message={error} />
	{/if}
</div>
