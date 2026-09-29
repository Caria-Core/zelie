<script lang="ts">
	import { RotateCw } from '@lucide/svelte';
	import { untrack } from 'svelte';
	import { ApiError } from '$lib/api';
	import { ask } from '$lib/ask.svelte';
	import { messageOf } from '$lib/errors';
	import { game, gameState, power, saveSettings } from '$lib/games.svelte';
	import { say, t } from '$lib/i18n';
	import Button from '$lib/ui/Button.svelte';
	import ErrorText from '$lib/ui/ErrorText.svelte';

	const g = $derived(game.info!);
	const phase = $derived(gameState(g));
	const up = $derived(phase === 'running' || phase === 'starting');

	// What is in the form. The page polls the server, so the form is filled
	// once and only reset by a save.
	let values = $state<Record<string, string>>({});
	let image = $state('');
	let ready = false;
	let busy = $state(false);
	let restarting = $state(false);
	let error = $state('');
	let bad = $state<{ env: string; text: string } | null>(null);
	let saved = $state(false);

	function reset() {
		values = Object.fromEntries(g.variables.filter((v) => v.editable).map((v) => [v.env, v.value]));
		image = g.image;
	}

	$effect(() => {
		g.id;
		untrack(() => {
			if (!ready) reset();
			ready = true;
		});
	});

	const changed = $derived(g.variables.filter((v) => v.editable && values[v.env] !== v.value).map((v) => v.env));
	const dirty = $derived(changed.length > 0 || image !== g.image);

	async function save() {
		busy = true;
		error = '';
		bad = null;
		saved = false;
		try {
			const variables: Record<string, string> = {};
			for (const env of changed) variables[env] = values[env];
			await saveSettings(g.id, { image: image !== g.image ? image : undefined, variables });
			reset();
			saved = true;
		} catch (err) {
			if (err instanceof ApiError && err.msg.code === 'game.bad_variable' && typeof err.msg.params?.name === 'string') {
				bad = { env: err.msg.params.name, text: say(err.msg) };
			} else {
				error = messageOf(err);
			}
		} finally {
			busy = false;
		}
	}

	async function restart() {
		const ok = await ask({ title: t('game.restartConfirm', { server: g.id }), text: t('game.restartConfirmText'), action: t('game.restart') });
		if (!ok) return;
		restarting = true;
		error = '';
		try {
			await power(g.id, 'restart');
			saved = false;
		} catch (err) {
			error = messageOf(err);
		} finally {
			restarting = false;
		}
	}
</script>

{#snippet heading(title: string, lead: string)}
	<div>
		<h2 class="font-medium">{title}</h2>
		<p class="text-sm text-muted">{lead}</p>
	</div>
{/snippet}

<form
	class="flex max-w-2xl flex-col gap-10"
	onsubmit={(e) => {
		e.preventDefault();
		if (dirty && !busy) save();
	}}
>
	<section class="flex flex-col gap-3">
		{@render heading(t('startup.command'), t('startup.commandLead'))}
		<pre class="overflow-x-auto rounded-2xl bg-panel p-4 font-mono text-[13px] leading-relaxed break-all whitespace-pre-wrap">{g.startup_preview}</pre>
	</section>

	<section class="flex flex-col gap-3">
		{@render heading(t('startup.image'), t('startup.imageLead'))}
		{#if g.images.length > 1}
			<select bind:value={image} aria-label={t('startup.image')} class="h-10 w-full min-w-0 rounded-xl border border-line bg-bg px-3 font-mono text-sm outline-none transition focus:border-muted">
				{#each g.images as img (img.ref)}
					<option value={img.ref}>{img.label === img.ref ? img.ref : `${img.label} (${img.ref})`}</option>
				{/each}
			</select>
		{:else}
			<p class="font-mono text-sm break-all">{g.image}</p>
		{/if}
	</section>

	<section class="flex flex-col gap-3">
		{@render heading(t('startup.variables'), t('startup.variablesLead'))}
		{#if g.variables.length}
			<ul class="flex flex-col divide-y divide-line rounded-2xl border border-line">
				{#each g.variables as v (v.env)}
					{@const id = `var-${v.env}`}
					<li class="flex flex-col gap-1.5 px-4 py-3">
						<div class="flex flex-wrap items-baseline justify-between gap-x-4 gap-y-1">
							<label for={v.editable ? id : undefined} class="text-[15px] font-medium">{v.name}</label>
							<span class="font-mono text-xs text-muted">{v.env}</span>
						</div>
						{#if v.description}<p class="text-sm text-muted">{v.description}</p>{/if}
						{#if v.editable}
							<input
								{id}
								bind:value={values[v.env]}
								oninput={() => bad?.env === v.env && (bad = null)}
								autocomplete="off"
								autocapitalize="off"
								spellcheck="false"
								aria-invalid={bad?.env === v.env}
								aria-describedby={bad?.env === v.env ? `${id}-error` : undefined}
								class="h-10 w-full min-w-0 rounded-xl border bg-bg px-3.5 font-mono text-sm outline-none transition focus:border-muted {bad?.env === v.env ? 'border-danger' : 'border-line'}"
							/>
							{#if bad?.env === v.env}<p id="{id}-error" role="alert" class="text-sm text-danger">{bad.text}</p>{/if}
							{#if v.default !== values[v.env]}
								<p class="text-xs text-muted">{t('startup.default', { value: v.default || t('startup.empty') })}</p>
							{/if}
						{:else}
							<p class="font-mono text-sm break-all">{v.value || '—'}</p>
							<p class="text-xs text-muted">{t('startup.locked')}</p>
						{/if}
					</li>
				{/each}
			</ul>
		{:else}
			<p class="text-sm text-muted">{t('startup.noVariables')}</p>
		{/if}
	</section>

	<div class="flex flex-col gap-3">
		<ErrorText message={error} />
		<div class="flex flex-wrap items-center gap-3">
			<Button type="submit" {busy} disabled={!dirty}>{t('startup.save')}</Button>
			{#if saved && !dirty}<span class="text-sm text-muted" role="status">{t('startup.saved')}</span>{/if}
		</div>
		{#if saved && !dirty && up}
			<div class="flex flex-wrap items-center justify-between gap-3 rounded-xl border border-line px-4 py-3">
				<p class="text-sm">{t('startup.restartNote')}</p>
				<Button type="button" kind="secondary" class="!h-8 !px-4" busy={restarting} onclick={restart}><RotateCw size={14} strokeWidth={1.75} />{t('startup.restart')}</Button>
			</div>
		{/if}
	</div>
</form>
