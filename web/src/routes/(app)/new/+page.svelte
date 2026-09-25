<script lang="ts">
	import { goto } from '$app/navigation';
	import { api } from '$lib/api';
	import { reload } from '$lib/containers.svelte';
	import { messageOf } from '$lib/errors';
	import { t } from '$lib/i18n';
	import Button from '$lib/ui/Button.svelte';
	import ErrorText from '$lib/ui/ErrorText.svelte';
	import Field from '$lib/ui/Field.svelte';
	import Lead from '$lib/ui/Lead.svelte';

	let id = $state('');
	let image = $state('');
	let memory = $state('512');
	let cpus = $state('1');
	let network = $state('');
	let error = $state('');
	let busy = $state(false);

	async function submit(e: SubmitEvent) {
		e.preventDefault();
		busy = true;
		error = '';
		try {
			await api('POST', '/containers', {
				id,
				image,
				network,
				memory_mb: Number(memory),
				cpus: Number(cpus)
			});
			await reload();
			await goto('/c/' + id);
		} catch (err) {
			error = messageOf(err);
		} finally {
			busy = false;
		}
	}
</script>

<div class="flex max-w-md flex-col gap-8">
	<Lead title={t('new.title')} text={' ' + t('new.lead')} />
	<form class="flex flex-col gap-4" onsubmit={submit}>
		<Field label={t('new.name')} hint={t('new.nameHint')} pattern="[a-z0-9][a-z0-9\-]*" required autocomplete="off" bind:value={id} />
		<Field label={t('new.image')} hint={t('new.imageHint')} required autocomplete="off" bind:value={image} />
		<div class="grid grid-cols-2 gap-4">
			<Field label={t('new.memory')} type="number" min="16" step="16" required bind:value={memory} />
			<Field label={t('new.cpus')} type="number" min="0.1" step="0.1" required bind:value={cpus} />
		</div>
		<Field label={t('new.network')} hint={t('new.networkHint')} pattern="[a-z0-9][a-z0-9\-]*" autocomplete="off" bind:value={network} />
		<ErrorText message={error} />
		<div class="mt-2 flex items-center gap-4">
			<Button type="submit" {busy}>{t('new.submit')}</Button>
			{#if busy}<span class="text-sm text-muted">{t('new.starting')}</span>{/if}
		</div>
	</form>
</div>
