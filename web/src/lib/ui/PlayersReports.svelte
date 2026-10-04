<script lang="ts">
	import { ChevronDown, ChevronUp } from '@lucide/svelte';
	import { untrack } from 'svelte';
	import { api } from '$lib/api';
	import { messageOf } from '$lib/errors';
	import { ago } from '$lib/format';
	import { t } from '$lib/i18n';
	import { stamp, type ReportTarget } from '$lib/players';
	import { view } from '$lib/players.svelte';
	import ErrorText from './ErrorText.svelte';

	let { app }: { app: string } = $props();
	let targets = $state<ReportTarget[] | null>(null);
	let open = $state<string[]>([]);
	let error = $state('');

	$effect(() => {
		app;
		view.tick;
		untrack(async () => {
			try {
				targets = (await api<{ targets: ReportTarget[] }>('GET', `/games/${encodeURIComponent(app)}/reports`)).targets;
			} catch (err) {
				error = messageOf(err);
			}
		});
	});
</script>

<div class="flex flex-col gap-4">
	<p class="text-sm text-muted">{t('players.reportsLead')}</p>
	<ErrorText message={error} />
	{#if targets && targets.length === 0}
		<p class="rounded-2xl border border-line px-4 py-6 text-center text-sm text-muted">{t('players.noReports')}</p>
	{:else if targets}
		<ul class="flex flex-col divide-y divide-line rounded-2xl border border-line">
			{#each targets as r (r.target_id)}
				{@const shown = open.includes(r.target_id)}
				<li class="flex flex-col">
					<div class="flex items-center justify-between gap-3 px-4 py-3">
						<div class="flex min-w-0 flex-col gap-0.5">
							<button class="w-fit max-w-full truncate text-left text-[15px] font-medium underline decoration-line underline-offset-2 hover:decoration-fg" onclick={() => (view.detail = r.target_id)}>{r.target_name || r.target_id}</button>
							<span class="text-sm text-muted">{t('players.reportSummary', { count: r.count, reporters: r.reporters, when: ago(r.last_at) })}</span>
						</div>
						<button
							class="rounded-lg p-2 text-muted transition hover:bg-hover hover:text-fg"
							aria-expanded={shown}
							aria-label={t('players.toggleReports')}
							onclick={() => (open = shown ? open.filter((o) => o !== r.target_id) : [...open, r.target_id])}
						>
							{#if shown}<ChevronUp size={18} strokeWidth={1.75} />{:else}<ChevronDown size={18} strokeWidth={1.75} />{/if}
						</button>
					</div>
					{#if shown}
						<ul class="flex flex-col gap-3 border-t border-line bg-panel px-4 py-3">
							{#each r.items as i, n (n)}
								<li class="flex flex-col gap-0.5 text-sm">
									<span class="text-muted">{t('players.reportBy', { name: i.reporter_name || i.reporter_id, when: stamp(i.at) })}</span>
									{#if i.subject}<span class="font-medium">{i.subject}</span>{/if}
									{#if i.message}<span class="break-words">{i.message}</span>{/if}
								</li>
							{/each}
						</ul>
					{/if}
				</li>
			{/each}
		</ul>
	{/if}
</div>
