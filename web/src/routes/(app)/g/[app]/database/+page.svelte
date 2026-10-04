<script lang="ts">
	import { Check, Copy } from '@lucide/svelte';
	import { page } from '$app/state';
	import { apps, engineLabel, type Link } from '$lib/apps.svelte';
	import { t } from '$lib/i18n';
	import AppIcon from '$lib/ui/AppIcon.svelte';
	import Button from '$lib/ui/Button.svelte';
	import Links from '$lib/ui/Links.svelte';
	import PasswordRow from '$lib/ui/PasswordRow.svelte';

	const id = $derived(page.params.app ?? '');
	const app = $derived(apps.list.find((a) => a.id === id));
	let links = $state<Link[]>([]);
	let copied = $state('');

	// The first variable of each link is its URL, the one that says it all.
	const mainVars = $derived(links.map((l) => l.vars[0]).filter(Boolean));

	// Redis has one password and no user or database name to put in a config.
	const fields = (l: Link) => {
		const db = apps.list.find((a) => a.id === l.db);
		return [
			{ label: t('db.host'), value: l.db },
			{ label: t('db.port'), value: String(db?.port ?? '') },
			...(l.engine === 'redis'
				? []
				: [
						{ label: t('db.database'), value: 'app' },
						{ label: t('db.user'), value: 'app' }
					])
		];
	};

	async function copy(key: string, text: string) {
		try {
			await navigator.clipboard.writeText(text);
			copied = key;
			setTimeout(() => copied === key && (copied = ''), 2000);
		} catch {
			// It is on screen to select by hand.
		}
	}
</script>

{#if app}
	<div class="flex max-w-2xl flex-col gap-10">
		<Links {app} onchange={(l) => (links = l)} />

		{#each links as l (l.db)}
			{@const db = apps.list.find((a) => a.id === l.db)}
			<section class="flex flex-col gap-3">
				<div class="flex items-center gap-3">
					<AppIcon database size="sm" />
					<div class="min-w-0">
						<h3 class="truncate font-medium">{l.db}</h3>
						<p class="text-sm text-muted">{t('gameDb.cardLead', { db: `${engineLabel[l.engine]} ${db?.engine_version ?? ''}`.trim() })}</p>
					</div>
				</div>
				<dl class="divide-y divide-line overflow-hidden rounded-2xl border border-line">
					{#each fields(l) as f (f.label)}
						<div class="flex items-center gap-3 bg-bg py-1.5 pr-2 pl-4">
							<dt class="w-24 shrink-0 text-sm text-muted">{f.label}</dt>
							<dd class="min-w-0 flex-1 font-mono text-[15px] break-all">{f.value}</dd>
							<Button kind="quiet" class="!h-8 shrink-0 !px-2" title={t('db.copy')} aria-label={t('db.copy')} onclick={() => copy(l.db + f.label, f.value)}>
								{#if copied === l.db + f.label}<Check size={16} />{:else}<Copy size={16} />{/if}
							</Button>
						</div>
					{/each}
				</dl>
				<PasswordRow db={l.db} />
				{#if db && db.state !== 'running'}<p class="text-sm text-muted">{t('gameDb.notRunning', { db: l.db })}</p>{/if}
			</section>
		{/each}

		{#if links.length > 0}
			<p class="text-sm text-muted">{t('gameDb.why', { vars: mainVars.join(', ') })} {t('gameDb.restart')}</p>
		{/if}
	</div>
{/if}
