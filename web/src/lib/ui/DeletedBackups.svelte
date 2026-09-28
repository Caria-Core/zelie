<script lang="ts">
	import { Archive } from '@lucide/svelte';
	import { api } from '$lib/api';
	import { apps, engineLabel } from '$lib/apps.svelte';
	import type { Backup } from '$lib/backups';
	import { date } from '$lib/format';
	import { t } from '$lib/i18n';
	import Button from '$lib/ui/Button.svelte';

	// Backups outlive a deleted app or database for a while; this is where
	// they can be found again. onuse gets the newest backup of the one
	// picked, to make a new one like it under the same name.
	let { kind, onuse }: { kind: 'app' | 'database'; onuse: (newest: Backup) => void } = $props();

	let deleted = $state<{ app: string; list: Backup[] }[]>([]);
	$effect(() => {
		const databases = kind === 'database';
		api<Backup[]>('GET', '/backups/deleted').then(
			(list) => {
				const by = new Map<string, Backup[]>();
				for (const b of list) if (!!b.engine === databases) by.set(b.app, [...(by.get(b.app) ?? []), b]);
				deleted = [...by].map(([app, l]) => ({ app, list: l }));
			},
			() => (deleted = [])
		);
	});

	const what = (b: Backup) => (b.engine ? engineLabel[b.engine] : (b.volumes ?? []).map((v) => '/' + v).join(', '));
</script>

{#if deleted.length > 0}
	<section class="flex flex-col gap-3">
		<div>
			<h2 class="font-medium">{kind === 'database' ? t('backups.deletedTitle') : t('backups.deletedAppsTitle')}</h2>
			<p class="text-sm text-muted">{kind === 'database' ? t('backups.deletedLead') : t('backups.deletedAppsLead')}</p>
		</div>
		<ul class="flex flex-col divide-y divide-line rounded-2xl border border-line">
			{#each deleted as d (d.app)}
				<li class="flex flex-wrap items-center justify-between gap-3 p-4">
					<div class="flex min-w-0 items-center gap-3">
						<span class="grid size-9 shrink-0 place-items-center rounded-xl bg-selected"><Archive size={18} strokeWidth={1.75} /></span>
						<div class="min-w-0">
							<p class="truncate text-[15px]">{d.app} <span class="text-muted">· {what(d.list[0])}</span></p>
							<p class="text-sm text-muted">
								{t('backups.deletedItem', {
									n: d.list.length,
									when: date(d.list[0].created_at),
									until: date(d.list.reduce((a, b) => (a.keep_until > b.keep_until ? a : b)).keep_until)
								})}
							</p>
						</div>
					</div>
					<Button
						kind="secondary"
						class="!h-8 !px-3 text-sm"
						onclick={() => onuse(d.list[0])}
						disabled={apps.list.some((a) => a.id === d.app)}>{t('backups.useName')}</Button
					>
				</li>
			{/each}
		</ul>
	</section>
{/if}
