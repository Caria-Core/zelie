<script lang="ts">
	import { EyeOff, Lock, Plus, X } from '@lucide/svelte';
	import { api } from '$lib/api';
	import { current, load } from '$lib/current.svelte';
	import { messageOf } from '$lib/errors';
	import { t } from '$lib/i18n';
	import Button from '$lib/ui/Button.svelte';
	import ErrorText from '$lib/ui/ErrorText.svelte';
	import Links from '$lib/ui/Links.svelte';

	// keep marks a saved secret whose value the browser never saw.
	type Row = { name: string; value: string; secret: boolean; keep: boolean };

	const app = $derived(current.app!);
	let rows = $state<Row[]>([]);
	let error = $state('');
	let saved = $state(false);
	let busy = $state(false);

	// Rows are copied once per app, so a refresh while typing loses nothing.
	let loadedFor = '';
	$effect(() => {
		if (app.id === loadedFor) return;
		loadedFor = app.id;
		rows = app.env.map((v) => ({ ...v, keep: v.secret }));
	});

	async function save(e: SubmitEvent) {
		e.preventDefault();
		busy = true;
		error = '';
		saved = false;
		try {
			const body = rows
				.filter((r) => r.name.trim() !== '')
				.map((r) => ({ name: r.name.trim(), value: r.keep ? '' : r.value, secret: r.secret, keep: r.keep }));
			await api('PUT', `/apps/${app.id}/env`, body);
			loadedFor = '';
			await load(app.id);
			saved = true;
		} catch (err) {
			error = messageOf(err);
		} finally {
			busy = false;
		}
	}

	// Pasting a whole .env file into a name field fills in every line.
	function paste(e: ClipboardEvent, i: number) {
		const text = e.clipboardData?.getData('text') ?? '';
		if (!text.includes('\n') && !text.includes('=')) return;
		e.preventDefault();
		const parsed = text
			.split('\n')
			.map((l) => l.trim())
			.filter((l) => l && !l.startsWith('#') && l.includes('='))
			.map((l) => {
				const at = l.indexOf('=');
				const value = l.slice(at + 1).replace(/^(['"])(.*)\1$/, '$2');
				return { name: l.slice(0, at).replace(/^export\s+/, ''), value, secret: false, keep: false };
			});
		rows.splice(i, 1, ...parsed);
	}

	// Not monospace: Chromium clips the underscore of DejaVu Sans Mono, the
	// usual monospace font on Linux, inside text fields, and variable names
	// are full of underscores.
	const input = 'h-9 w-full rounded-lg border border-line bg-bg px-3 text-sm outline-none focus:border-muted';
</script>

<div class="flex max-w-3xl flex-col gap-10">
	<Links {app} />
	<form class="flex flex-col gap-4" onsubmit={save}>
		<h2 class="font-medium">{t('env.own')}</h2>
		<p class="text-sm text-muted">{t('env.lead')} {t('env.secretHint')}</p>
		{#if rows.length === 0}<p class="text-sm text-muted/80">{t('env.empty')}</p>{/if}
		<div class="flex flex-col gap-2">
			{#each rows as row, i (i)}
				<div class="grid grid-cols-[minmax(0,2fr)_minmax(0,3fr)_auto_auto] items-center gap-2">
					<input class={input} placeholder={t('env.name')} aria-label={t('env.name')} autocomplete="off" spellcheck="false" bind:value={row.name} onpaste={(e) => paste(e, i)} />
					{#if row.keep}
						<div class="flex h-9 items-center justify-between gap-2 rounded-lg border border-dashed border-line px-3 text-sm text-muted">
							<span class="inline-flex items-center gap-1.5"><EyeOff size={14} />{t('env.hidden')}</span>
							<button type="button" class="hover:text-fg" onclick={() => ((row.keep = false), (row.value = ''))}>{t('env.replace')}</button>
						</div>
					{:else}
						<input class={input} type={row.secret ? 'password' : 'text'} placeholder={t('env.value')} aria-label={t('env.value')} autocomplete="off" spellcheck="false" bind:value={row.value} />
					{/if}
					<label class="inline-flex cursor-pointer items-center gap-1.5 text-sm {row.secret ? 'text-fg' : 'text-muted'}" title={t('env.secretHint')}>
						<input type="checkbox" class="sr-only" bind:checked={row.secret} disabled={row.keep} />
						<Lock size={14} strokeWidth={row.secret ? 2.25 : 1.5} />{t('env.secret')}
					</label>
					<button type="button" class="rounded-md p-1 text-muted hover:text-danger" aria-label={t('env.remove', { name: row.name })} onclick={() => rows.splice(i, 1)}
						><X size={16} /></button
					>
				</div>
			{/each}
		</div>
		<div class="flex flex-wrap items-center gap-3">
			<Button kind="secondary" type="button" onclick={() => rows.push({ name: '', value: '', secret: false, keep: false })}><Plus size={16} />{t('env.add')}</Button>
			<Button type="submit" {busy}>{t('env.save')}</Button>
			{#if saved}<span role="status" class="text-sm text-ok">{t('env.saved')}</span>{/if}
		</div>
		<ErrorText message={error} />
	</form>
</div>
