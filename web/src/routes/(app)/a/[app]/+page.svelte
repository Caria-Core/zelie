<script lang="ts">
	import { inProgress, shortVersion, type Deployment } from '$lib/apps.svelte';
	import { current, load, rollback } from '$lib/current.svelte';
	import { messageOf } from '$lib/errors';
	import ErrorText from '$lib/ui/ErrorText.svelte';
	import { ago } from '$lib/format';
	import { t } from '$lib/i18n';
	import DeployState from '$lib/ui/DeployState.svelte';
	import LogStream from '$lib/ui/LogStream.svelte';

	const app = $derived(current.app!);
	// The newest deployment's log is open while it runs, so the build can
	// be watched without a click.
	let opened = $state<number | null>(null);
	$effect(() => {
		const latest = app.deployments[0];
		if (inProgress(latest) && opened === null) opened = latest.id;
	});

	let error = $state('');
	async function back(d: Deployment) {
		if (!confirm(t('app.rollbackConfirm', { version: shortVersion(d.version) }))) return;
		error = '';
		try {
			await rollback(app.id, d.id);
		} catch (err) {
			error = messageOf(err);
		}
	}

	const causes: Partial<Record<Deployment['cause'], string>> = {
		push: t('deploy.push'),
		restart: t('deploy.restart'),
		rollback: t('deploy.rollback'),
		recover: t('deploy.recover')
	};

	function took(d: Deployment): string {
		if (!d.finished_at) return '';
		const s = Math.max(1, Math.round((new Date(d.finished_at).getTime() - new Date(d.created_at).getTime()) / 1000));
		return t('app.took', { time: s < 60 ? `${s}s` : `${Math.floor(s / 60)}m ${s % 60}s` });
	}
</script>

<ErrorText message={error} />
{#if app.deployments.length === 0}
	<p class="text-muted">{t('app.noDeployments')}</p>
{:else}
	<ul class="flex flex-col divide-y divide-line rounded-2xl border border-line">
		{#each app.deployments as d (d.id)}
			<li class="flex flex-col gap-3 px-4 py-3">
				<div class="flex flex-wrap items-center justify-between gap-x-4 gap-y-2">
					<div class="flex min-w-0 items-center gap-3">
						<DeployState state={d.state} />
						<span class="shrink-0 font-mono text-sm">{shortVersion(d.version)}</span>
						{#if d.message}<span class="min-w-0 truncate text-sm" title={d.message}>{d.message}</span>{/if}
						<span class="shrink-0 text-sm text-muted"
							>{ago(d.created_at)}{causes[d.cause] ? ' · ' + causes[d.cause] : ''}{d.finished_at ? ' · ' + took(d) : ''}</span
						>
					</div>
					<div class="flex items-center gap-4">
						{#if d.state === 'replaced' && d.kept && !inProgress(app.deployments[0])}
							<button class="text-sm text-muted hover:text-fg" onclick={() => back(d)}>{t('app.rollback')}</button>
						{/if}
						<button class="text-sm text-muted hover:text-fg" onclick={() => (opened = opened === d.id ? -1 : d.id)}
							>{opened === d.id ? t('app.hideLog') : t('app.buildLog')}</button
						>
					</div>
				</div>
				{#if d.error}<p class="text-sm text-danger">{d.error}</p>{/if}
				{#if opened === d.id}
					<LogStream url="/api/apps/{app.id}/deployments/{d.id}/log" height="max-h-[50vh] min-h-24" ondone={() => load(app.id)} />
				{/if}
			</li>
		{/each}
	</ul>
{/if}
