<script lang="ts">
	import { ArrowUpRight } from '@lucide/svelte';
	import { apps, shortVersion } from '$lib/apps.svelte';
	import { ago } from '$lib/format';
	import { t } from '$lib/i18n';
	import AppIcon from '$lib/ui/AppIcon.svelte';
	import DeployState from '$lib/ui/DeployState.svelte';
	import Lead from '$lib/ui/Lead.svelte';
	import SourceChoice from '$lib/ui/SourceChoice.svelte';
	import StateDot from '$lib/ui/StateDot.svelte';

	const live = $derived(apps.list.filter((a) => a.state === 'running').length);
</script>

{#if apps.loaded}
	{#if apps.list.length === 0}
		<div class="flex max-w-2xl flex-col gap-8">
			<Lead title={t('home.title')} text={' ' + t('home.lead')} />
			<SourceChoice />
		</div>
	{:else}
		<div class="flex flex-col gap-8">
			<Lead title={t('home.count')} text={' ' + t('home.countLead', { n: live })} />
			<ul class="grid gap-3 sm:grid-cols-2 xl:grid-cols-3">
				{#each apps.list as a (a.id)}
					<li class="relative flex flex-col gap-4 rounded-2xl border border-line p-4 transition hover:bg-hover">
						<div class="flex items-center gap-3">
							<AppIcon source={a.source} />
							<div class="min-w-0 flex-1">
								<a href="/a/{a.id}" class="block truncate font-medium after:absolute after:inset-0">{a.id}</a>
								<p class="truncate text-sm text-muted">{a.source === 'github' ? a.repo : a.image}</p>
							</div>
							<StateDot state={a.state} />
						</div>
						<div class="flex items-center justify-between gap-2 text-sm text-muted">
							{#if a.domain}
								<a
									href="https://{a.domain}"
									target="_blank"
									rel="noopener noreferrer"
									class="relative z-10 inline-flex min-w-0 items-center gap-1 hover:text-fg"
									><span class="truncate">{a.domain}</span><ArrowUpRight size={14} class="shrink-0" /></a
								>
							{:else}<span></span>{/if}
							{#if a.latest}
								<span class="flex shrink-0 items-center gap-2">
									<span class="font-mono text-xs">{shortVersion(a.latest.version)}</span>
									<DeployState state={a.latest.state} />
								</span>
							{/if}
						</div>
						{#if a.latest}<p class="-mt-2 text-xs text-muted/80">{ago(a.latest.created_at)}</p>{/if}
					</li>
				{/each}
			</ul>
		</div>
	{/if}
{/if}
