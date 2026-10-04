<script lang="ts">
	import { Archive, KeyRound, ShieldCheck } from '@lucide/svelte';
	import { api } from '$lib/api';
	import { engineLabel, type App } from '$lib/apps.svelte';
	import type { Backup, Backups } from '$lib/backups';
	import { ago } from '$lib/format';
	import { t } from '$lib/i18n';
	import Links from './Links.svelte';
	import OutsideAccess from './OutsideAccess.svelte';
	import PasswordRow from './PasswordRow.svelte';

	// How apps reach a database. Zelie hands the password to the apps linked
	// to it; an administrator can also have it shown, after a fresh confirmation.
	let { app }: { app: App } = $props();
	const user = 'app';
	const rows = $derived([
		{ label: t('db.host'), value: app.id },
		{ label: t('db.port'), value: String(app.port) },
		...(app.engine === 'redis'
			? []
			: [
					{ label: t('db.user'), value: user },
					{ label: t('db.database'), value: user }
				])
	]);

	// The newest backup, made or tried, so a failing one is seen here too.
	let last = $state<Backup | null | undefined>(undefined);
	$effect(() => {
		api<Backups>('GET', `/apps/${app.id}/backups`).then(
			(b) => (last = b.backups.find((x) => x.state !== 'running') ?? null),
			() => (last = undefined)
		);
	});
</script>

<div class="flex max-w-2xl flex-col gap-10">
	<section class="flex flex-col gap-4">
		<div>
			<h2 class="font-medium">{t('db.connection')}</h2>
			<p class="text-sm text-muted">{t('db.connectionLead', { engine: app.engine ? engineLabel[app.engine] : '' })}</p>
		</div>
		<dl class="grid grid-cols-2 gap-px overflow-hidden rounded-2xl border border-line bg-line sm:grid-cols-4">
			{#each rows as r (r.label)}
				<div class="flex flex-col gap-1 bg-bg px-4 py-3">
					<dt class="text-sm text-muted">{r.label}</dt>
					<dd class="truncate font-mono text-[15px]">{r.value}</dd>
				</div>
			{/each}
		</dl>
		<PasswordRow db={app.id} />
		<ul class="flex flex-col gap-2 text-sm text-muted">
			<li class="flex gap-2.5"><ShieldCheck size={16} strokeWidth={1.75} class="mt-0.5 shrink-0 text-fg" />{t('db.private')}</li>
			<li class="flex gap-2.5"><KeyRound size={16} strokeWidth={1.75} class="mt-0.5 shrink-0 text-fg" />{t('db.passwordShort')}</li>
			{#if last !== undefined}
				<li class="flex gap-2.5">
					<Archive size={16} strokeWidth={1.75} class="mt-0.5 shrink-0 {last?.state === 'failed' ? 'text-danger' : 'text-fg'}" />
					<a href="/a/{app.id}/backups" class="hover:text-fg {last?.state === 'failed' ? 'text-danger' : ''}"
						>{#if !last}{t('backups.none')}{:else if last.state === 'failed'}{t('backups.lastFailed', { when: ago(last.created_at) })}{:else}{t(
								'backups.last',
								{ when: ago(last.created_at) }
							)}{/if}</a
					>
				</li>
			{/if}
		</ul>
	</section>
	<Links {app} />
	<OutsideAccess {app} />
</div>
