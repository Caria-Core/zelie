<script lang="ts">
	import { KeyRound, ShieldCheck } from '@lucide/svelte';
	import { engineLabel, type App } from '$lib/apps.svelte';
	import { t } from '$lib/i18n';
	import Links from './Links.svelte';

	// How apps reach a database. The password is never shown: Zelie hands
	// it to the apps linked to the database, and to nobody else.
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
		<ul class="flex flex-col gap-2 text-sm text-muted">
			<li class="flex gap-2.5"><ShieldCheck size={16} strokeWidth={1.75} class="mt-0.5 shrink-0 text-fg" />{t('db.private')}</li>
			<li class="flex gap-2.5"><KeyRound size={16} strokeWidth={1.75} class="mt-0.5 shrink-0 text-fg" />{t('db.passwordShort')}</li>
		</ul>
	</section>
	<Links {app} />
</div>
