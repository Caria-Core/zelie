<script lang="ts">
	import { ChevronRight } from '@lucide/svelte';
	import { t } from '$lib/i18n';
	import AppIcon from './AppIcon.svelte';

	// Where an app's code comes from. A database is not one; it has its own
	// page, linked below for anyone who looks for it here.
	const choices = [
		{ source: 'github', href: '/new?source=github', title: t('source.github'), lead: t('source.githubLead') },
		{ source: 'image', href: '/new?source=image', title: t('source.image'), lead: t('source.imageLead') }
	] as const;
</script>

<div class="grid gap-3 sm:grid-cols-2">
	{#each choices as c (c.source)}
		<a href={c.href} class="group flex items-start gap-4 rounded-2xl border border-line p-5 transition hover:bg-hover">
			<AppIcon source={c.source} size="lg" />
			<div class="min-w-0 flex-1">
				<p class="flex items-center gap-1 font-medium">
					{c.title}<ChevronRight size={16} class="text-muted transition group-hover:translate-x-0.5" />
				</p>
				<p class="mt-1 text-sm text-muted">{c.lead}</p>
			</div>
		</a>
	{/each}
</div>
<p class="text-sm text-muted">
	{t('source.needDatabase')}
	<a href="/new/database" class="text-fg underline decoration-line underline-offset-4 hover:decoration-fg">{t('source.createDatabase')}</a>
</p>
<p class="-mt-4 text-sm text-muted">
	{t('source.needGame')}
	<a href="/new/game" class="text-fg underline decoration-line underline-offset-4 hover:decoration-fg">{t('source.createGame')}</a>
</p>
