<script lang="ts">
	import { game } from '$lib/games.svelte';
	import { t } from '$lib/i18n';

	const g = $derived(game.info!);
</script>

{#snippet heading(title: string, lead: string)}
	<div>
		<h2 class="font-medium">{title}</h2>
		<p class="text-sm text-muted">{lead}</p>
	</div>
{/snippet}

<div class="flex max-w-2xl flex-col gap-10">
	<section class="flex flex-col gap-3">
		{@render heading(t('startup.command'), t('startup.commandLead'))}
		<pre class="overflow-x-auto rounded-2xl bg-panel p-4 font-mono text-[13px] leading-relaxed break-all whitespace-pre-wrap">{g.startup}</pre>
	</section>

	<section class="flex flex-col gap-3">
		{@render heading(t('startup.image'), t('startup.imageLead'))}
		<p class="font-mono text-sm break-all">{g.image}</p>
	</section>

	<section class="flex flex-col gap-3">
		{@render heading(t('startup.variables'), t('startup.variablesLead'))}
		{#if g.variables.length}
			<ul class="flex flex-col divide-y divide-line rounded-2xl border border-line">
				{#each g.variables as v (v.env)}
					<li class="flex flex-col gap-1 px-4 py-3">
						<div class="flex flex-wrap items-baseline justify-between gap-x-4 gap-y-1">
							<span class="text-[15px] font-medium">{v.name}</span>
							<span class="font-mono text-xs text-muted">{v.env}</span>
						</div>
						{#if v.description}<p class="text-sm text-muted">{v.description}</p>{/if}
						<p class="font-mono text-sm break-all">{v.value || '—'}</p>
						{#if !v.editable}<p class="text-xs text-muted">{t('startup.locked')}</p>{/if}
					</li>
				{/each}
			</ul>
		{:else}
			<p class="text-sm text-muted">{t('startup.noVariables')}</p>
		{/if}
	</section>
</div>
