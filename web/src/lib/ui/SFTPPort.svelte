<script lang="ts">
	import { CircleCheck } from '@lucide/svelte';
	import { onMount } from 'svelte';
	import { api } from '$lib/api';
	import { messageOf } from '$lib/errors';
	import { t } from '$lib/i18n';
	import Button from './Button.svelte';
	import ErrorText from './ErrorText.svelte';
	import Field from './Field.svelte';

	type Info = { port: number; host_key: string; running: boolean };

	let info = $state<Info | null>(null);
	let port = $state('');
	let saving = $state(false);
	let saved = $state(false);
	let error = $state('');

	onMount(async () => {
		try {
			info = await api<Info>('GET', '/sftp');
			port = String(info.port);
		} catch (err) {
			error = messageOf(err);
		}
	});

	async function save(e: SubmitEvent) {
		e.preventDefault();
		saving = true;
		saved = false;
		error = '';
		try {
			info = await api<Info>('PUT', '/sftp', { port: Number(port) });
			port = String(info.port);
			saved = true;
		} catch (err) {
			error = messageOf(err);
		} finally {
			saving = false;
		}
	}
</script>

{#if info}
	<section class="flex flex-col gap-4">
		<div>
			<h2 class="font-medium">{t('sftp.serverTitle')}</h2>
			<p class="text-sm text-muted">{t('sftp.serverLead')}</p>
		</div>
		<p class="text-sm {info.running ? 'text-muted' : 'text-danger'}">{info.running ? t('sftp.running', { port: info.port }) : t('sftp.notRunning')}</p>
		<form class="flex flex-col gap-4" onsubmit={save}>
			<Field label={t('sftp.port')} hint={t('sftp.portHint')} type="number" min="1024" max="65535" required bind:value={port} />
			<div class="flex items-center gap-3">
				<Button type="submit" busy={saving}>{t('sftp.savePort')}</Button>
				{#if saved}<CircleCheck size={16} strokeWidth={1.75} class="text-muted" />{/if}
			</div>
			<ErrorText message={error} />
		</form>
		{#if info.host_key}
			<div class="flex flex-col gap-1">
				<h3 class="text-sm font-medium">{t('sftp.hostKey')}</h3>
				<code class="font-mono text-sm break-all select-all">{info.host_key}</code>
				<p class="text-sm text-muted">{t('sftp.hostKeyHint')}</p>
			</div>
		{/if}
	</section>
{:else}
	<ErrorText message={error} />
{/if}
