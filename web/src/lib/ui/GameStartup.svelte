<script lang="ts">
	import { RotateCw } from '@lucide/svelte';
	import { untrack } from 'svelte';
	import { ApiError } from '$lib/api';
	import { ask } from '$lib/ask.svelte';
	import { messageOf } from '$lib/errors';
	import { game, gameState, power, saveSettings } from '$lib/games.svelte';
	import { say, t } from '$lib/i18n';
	import { session } from '$lib/session.svelte';
	import Button from '$lib/ui/Button.svelte';
	import ErrorText from '$lib/ui/ErrorText.svelte';

	const g = $derived(game.info!);
	const phase = $derived(gameState(g));
	const up = $derived(phase === 'running' || phase === 'starting');
	// The egg's locks are for other users; administrators may change anything.
	const admin = $derived(!!session.me?.admin);
	const other = '__other';

	// What is in the form. The page polls the server, so the form is filled
	// once and only reset by a save.
	let values = $state<Record<string, string>>({});
	// An image of the egg's list, or `other` with the reference typed below.
	let imageChoice = $state('');
	let imageOther = $state('');
	let startup = $state('');
	let ready = false;
	let busy = $state(false);
	let restarting = $state(false);
	let error = $state('');
	let bad = $state<{ env: string; text: string } | null>(null);
	let saved = $state(false);

	function reset() {
		values = Object.fromEntries(g.variables.filter((v) => v.editable && !v.port).map((v) => [v.env, v.value]));
		const listed = g.images.some((img) => img.ref === g.image);
		imageChoice = listed ? g.image : other;
		imageOther = listed ? '' : g.image;
		startup = g.startup;
	}

	$effect(() => {
		g.id;
		untrack(() => {
			if (!ready) reset();
			ready = true;
		});
	});

	const image = $derived(imageChoice === other ? imageOther.trim() : imageChoice);
	const changed = $derived(g.variables.filter((v) => v.editable && !v.port && values[v.env] !== v.value).map((v) => v.env));
	const startupChanged = $derived(admin && startup.trim() !== g.startup);
	const dirty = $derived(changed.length > 0 || (image !== g.image && !!image) || startupChanged);

	async function save() {
		busy = true;
		error = '';
		bad = null;
		saved = false;
		try {
			const variables: Record<string, string> = {};
			for (const env of changed) variables[env] = values[env];
			// The egg's own command is saved as empty, so it stays the egg's.
			const command = startup.trim() === g.egg_startup ? '' : startup.trim();
			await saveSettings(g.id, {
				image: image !== g.image ? image : undefined,
				startup: startupChanged ? command : undefined,
				variables
			});
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
		{@render heading(t('startup.command'), admin ? t('startup.commandAdminLead') : t('startup.commandLead'))}
		{#if admin}
			<textarea
				bind:value={startup}
				aria-label={t('startup.command')}
				rows="3"
				maxlength="4096"
				autocapitalize="off"
				spellcheck="false"
				class="min-h-24 w-full resize-y rounded-2xl border border-line bg-bg p-4 font-mono text-[13px] leading-relaxed outline-none transition focus:border-muted"
			></textarea>
			<div class="flex flex-wrap items-center justify-between gap-x-4 gap-y-1 text-xs text-muted">
				<span>{t('startup.commandPlaceholders')}</span>
				{#if startup.trim() !== g.egg_startup}
					<button type="button" class="underline decoration-line underline-offset-2 hover:text-fg hover:decoration-fg" onclick={() => (startup = g.egg_startup)}>{t('startup.resetCommand')}</button>
				{/if}
			</div>
			{#if !startupChanged}
				<pre class="overflow-x-auto rounded-2xl bg-panel p-4 font-mono text-[13px] leading-relaxed break-all whitespace-pre-wrap">{g.startup_preview}</pre>
			{/if}
		{:else}
			<pre class="overflow-x-auto rounded-2xl bg-panel p-4 font-mono text-[13px] leading-relaxed break-all whitespace-pre-wrap">{g.startup_preview}</pre>
		{/if}
	</section>

	<section class="flex flex-col gap-3">
		{@render heading(t('startup.image'), t('startup.imageLead'))}
		{#if g.images.length > 1 || admin}
			<select bind:value={imageChoice} aria-label={t('startup.image')} class="h-10 w-full min-w-0 rounded-xl border border-line bg-bg px-3 font-mono text-sm outline-none transition focus:border-muted">
				{#each g.images as img (img.ref)}
					<option value={img.ref}>{img.label === img.ref ? img.ref : `${img.label} (${img.ref})`}</option>
				{/each}
				{#if admin}<option value={other}>{t('startup.otherImage')}</option>{/if}
			</select>
			{#if imageChoice === other}
				<input
					bind:value={imageOther}
					aria-label={t('startup.otherImageField')}
					placeholder="ghcr.io/org/image:tag"
					autocomplete="off"
					autocapitalize="off"
					spellcheck="false"
					class="h-10 w-full min-w-0 rounded-xl border border-line bg-bg px-3.5 font-mono text-sm outline-none transition focus:border-muted"
				/>
			{/if}
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
							<label for={v.editable && !v.port ? id : undefined} class="text-[15px] font-medium">{v.name}</label>
							<span class="font-mono text-xs text-muted">{v.env}</span>
						</div>
						{#if v.description}<p class="text-sm text-muted">{v.description}</p>{/if}
						{#if v.port}
							<p class="font-mono text-sm break-all">{v.value || '—'}</p>
							<p class="text-xs text-muted">{t('startup.portVariable')} <a href="/g/{g.id}/network" class="underline hover:text-fg">{t('startup.portVariableLink')}</a></p>
						{:else if v.editable}
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
							{#if v.locked}<p class="text-xs text-muted">{t('startup.lockedForUsers')}</p>{/if}
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
