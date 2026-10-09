<script lang="ts">
	import { Check, Copy } from '@lucide/svelte';
	import { api } from '$lib/api';
	import { sensitive } from '$lib/confirm.svelte';
	import { Cancelled, messageOf } from '$lib/errors';
	import { t } from '$lib/i18n';
	import { session } from '$lib/session.svelte';
	import Button from './Button.svelte';
	import ErrorText from './ErrorText.svelte';

	// Lets an app connect to port 3306 of the server itself, where a MariaDB
	// installed outside Zelie listens. Only an administrator changes it, after
	// confirming; everyone else sees what it is set to.
	type HostAccess = { on: boolean; name: string; port: number };

	let { app }: { app: string } = $props();
	let x = $state<HostAccess | null>(null);
	let error = $state('');
	let busy = $state(false);
	let copied = $state(false);
	const admin = $derived(!!session.me?.admin);
	const address = $derived(x ? `${x.name}:${x.port}` : '');

	$effect(() => {
		const want = app;
		x = null;
		error = '';
		api<HostAccess>('GET', `/apps/${encodeURIComponent(want)}/host-access`)
			.then((r) => want === app && (x = r))
			.catch((err) => want === app && (error = messageOf(err)));
	});

	async function set(on: boolean) {
		busy = true;
		error = '';
		try {
			x = await sensitive(() => api<HostAccess>('PUT', `/apps/${encodeURIComponent(app)}/host-access`, { on }));
		} catch (err) {
			if (!(err instanceof Cancelled)) error = messageOf(err);
		} finally {
			busy = false;
		}
	}

	async function copy() {
		await navigator.clipboard.writeText(address);
		copied = true;
		setTimeout(() => (copied = false), 2000);
	}
</script>

{#if x || error}
	<section class="flex flex-col gap-4">
		<div>
			<h2 class="font-medium">{t('hostAccess.title')}</h2>
			<p class="text-sm text-muted">{t('hostAccess.lead')}</p>
		</div>

		{#if x}
			{#if admin}
				<label class="flex items-start gap-2.5 text-[15px]">
					<input type="checkbox" class="mt-1 size-4 accent-[var(--fg)]" checked={x.on} disabled={busy} onchange={(e) => set(e.currentTarget.checked)} />
					<span>{t('hostAccess.allow')}<span class="block text-sm text-muted">{t('hostAccess.allowHint')}</span></span>
				</label>
			{:else}
				<p class="text-sm text-muted">{x.on ? t('hostAccess.readOnlyOn') : t('hostAccess.readOnlyOff')}</p>
			{/if}

			{#if x.on}
				<div class="flex flex-col gap-2">
					<h3 class="text-sm font-medium">{t('hostAccess.addressTitle')}</h3>
					<div class="flex items-center gap-2 rounded-xl bg-panel px-3 py-2">
						<code class="min-w-0 flex-1 font-mono text-sm break-all">{address}</code>
						<Button kind="quiet" class="!h-8 shrink-0 !px-2" title={t('outside.copy')} aria-label={t('outside.copy')} onclick={copy}>
							{#if copied}<Check size={16} />{:else}<Copy size={16} />{/if}
						</Button>
					</div>
					<p class="text-sm text-muted">{t('hostAccess.addressLead')}</p>
				</div>
			{/if}
		{/if}
		<ErrorText message={error} />
	</section>
{/if}
