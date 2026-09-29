<script lang="ts">
	import { api } from '$lib/api';
	import { messageOf } from '$lib/errors';
	import type { PortRange } from '$lib/games.svelte';
	import { t } from '$lib/i18n';
	import Button from './Button.svelte';
	import ErrorText from './ErrorText.svelte';
	import Field from './Field.svelte';

	// Adds a block of ports to the pool. Zelie proposes one that nothing on
	// the machine uses.
	let { onadded, oncancel }: { onadded: () => void; oncancel?: () => void } = $props();

	let range = $state('');
	let suggestion = $state('');
	let adding = $state(false);
	let error = $state('');

	$effect(() => {
		api<PortRange>('GET', '/nodes/1/allocations/suggest').then(
			(r) => {
				suggestion = r.ports;
				range = r.ports;
			},
			(err) => {
				suggestion = '';
				error = messageOf(err);
			}
		);
	});

	async function add(e: SubmitEvent) {
		e.preventDefault();
		adding = true;
		error = '';
		try {
			await api('POST', '/nodes/1/allocations', { ports: range.trim() });
			onadded();
		} catch (err) {
			error = messageOf(err);
		} finally {
			adding = false;
		}
	}
</script>

<form class="flex flex-col gap-3 rounded-2xl border border-line p-4" onsubmit={add}>
	<p class="text-[15px]">
		<span class="font-medium">{suggestion ? t('game.new.found', { ports: suggestion.replace('-', '–') }) : t('game.new.foundNone')}</span>
		<span class="text-muted">{' ' + t('game.new.foundLead')}</span>
	</p>
	<Field label={t('game.new.poolPorts')} hint={t('game.new.poolPortsHint')} placeholder="25565-25664" required autocomplete="off" bind:value={range} />
	<ErrorText message={error} />
	<div class="flex items-center gap-3">
		<Button type="submit" busy={adding} disabled={!range.trim()}>{t('game.new.addPool')}</Button>
		{#if oncancel}<Button kind="quiet" type="button" onclick={oncancel}>{t('common.cancel')}</Button>{/if}
	</div>
</form>
