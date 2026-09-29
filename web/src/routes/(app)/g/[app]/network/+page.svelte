<script lang="ts">
	import { address, game } from '$lib/games.svelte';
	import { t } from '$lib/i18n';

	const g = $derived(game.info!);
</script>

<div class="flex max-w-2xl flex-col gap-6">
	<div>
		<h2 class="font-medium">{t('network.title')}</h2>
		<p class="text-sm text-muted">{t('network.lead')}</p>
	</div>
	<ul class="flex flex-col divide-y divide-line rounded-2xl border border-line">
		{#each g.ports as p (p.id)}
			<li class="flex flex-wrap items-center justify-between gap-x-4 gap-y-1 px-4 py-3">
				<span class="font-mono text-[15px] select-all">{address(p)}</span>
				{#if p.default}
					<span class="rounded-full bg-selected px-2.5 py-0.5 text-xs">{t('network.default')}</span>
				{/if}
			</li>
		{/each}
	</ul>
	<p class="text-sm text-muted">{t('network.defaultHint')}</p>
</div>
