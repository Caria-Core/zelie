<script lang="ts">
	import { Link2, Unlink } from '@lucide/svelte';
	import { api } from '$lib/api';
	import { apps, engineLabel, isDatabase, reload, type App, type Link } from '$lib/apps.svelte';
	import { restart } from '$lib/current.svelte';
	import { messageOf } from '$lib/errors';
	import { t } from '$lib/i18n';
	import AppIcon from './AppIcon.svelte';
	import Button from './Button.svelte';
	import ErrorText from './ErrorText.svelte';

	// The links of an app to its databases, or of a database to its apps:
	// the same links seen from either end.
	let { app }: { app: App } = $props();
	const fromDatabase = $derived(isDatabase(app));

	let links = $state<Link[] | null>(null);
	let error = $state('');
	let busy = $state(false);
	let choice = $state('');
	let prefix = $state('');
	// Apps whose variables changed and that have not restarted since.
	let pending = $state<string[]>([]);

	async function refresh() {
		links = await api<Link[]>('GET', `/apps/${app.id}/links`).catch((err) => {
			error = messageOf(err);
			return [];
		});
	}
	$effect(() => {
		app.id;
		pending = [];
		refresh();
	});

	// What can still be linked: databases for an app, apps for a database.
	const options = $derived(
		apps.list.filter((a) => isDatabase(a) !== fromDatabase && !links?.some((l) => l.db === a.id))
	);
	$effect(() => {
		if (!options.some((o) => o.id === choice)) choice = options[0]?.id ?? '';
	});

	// The app and the database of a link, whichever end this page is.
	const ends = (other: string) => (fromDatabase ? { app: other, db: app.id } : { app: app.id, db: other });

	async function act(fn: () => Promise<unknown>, changed: string) {
		busy = true;
		error = '';
		try {
			await fn();
			await refresh();
			const running = apps.list.find((a) => a.id === changed)?.state === 'running';
			if (running && !pending.includes(changed)) pending.push(changed);
		} catch (err) {
			error = messageOf(err);
		} finally {
			busy = false;
		}
	}

	function link(e: SubmitEvent) {
		e.preventDefault();
		const { app: a, db } = ends(choice);
		act(async () => {
			await api('POST', `/apps/${a}/links`, { db, prefix: prefix.trim().toUpperCase() });
			prefix = '';
		}, a);
	}

	function unlink(l: Link) {
		const { app: a, db } = ends(l.db);
		if (!confirm(t('links.unlinkConfirm', { app: a, db }))) return;
		act(() => api('DELETE', `/apps/${a}/links/${db}`), a);
	}

	async function apply(id: string) {
		busy = true;
		try {
			await restart(id);
			await reload();
			pending = pending.filter((p) => p !== id);
		} catch (err) {
			error = messageOf(err);
		} finally {
			busy = false;
		}
	}

	const appNamed = (id: string) => apps.list.find((a) => a.id === id);
	// A select inside its label would take the chosen option into its name.
	const uid = $props.id();
</script>

<section class="flex flex-col gap-4">
	<div>
		<h2 class="font-medium">{fromDatabase ? t('links.appsTitle') : t('links.dbTitle')}</h2>
		<p class="text-sm text-muted">{fromDatabase ? t('links.appsLead') : t('links.dbLead')}</p>
	</div>

	{#if links && links.length > 0}
		<ul class="flex flex-col gap-3">
			{#each links as l (l.db)}
				{@const other = appNamed(l.db)}
				<li class="flex flex-col gap-3 rounded-2xl border border-line p-4">
					<div class="flex items-center justify-between gap-3">
						<a href="/a/{l.db}" class="flex min-w-0 items-center gap-3 hover:underline">
							<AppIcon source={other?.source ?? 'image'} database={!fromDatabase} />
							<span class="min-w-0">
								<span class="block truncate font-medium">{l.db}</span>
								<span class="block text-sm text-muted"
									>{fromDatabase ? t('links.app') : `${engineLabel[l.engine]} ${other?.engine_version ?? ''}`}{l.prefix
										? ' · ' + t('links.prefix', { prefix: l.prefix })
										: ''}</span
								>
							</span>
						</a>
						<Button kind="quiet" class="!h-9 !px-2 hover:!text-danger" disabled={busy} title={t('links.unlink')} aria-label={t('links.unlink')} onclick={() => unlink(l)}
							><Unlink size={16} /></Button
						>
					</div>
					<div class="flex flex-wrap gap-1.5">
						{#each l.vars as v (v)}<code class="rounded-md bg-selected px-1.5 py-0.5 text-xs">{v}</code>{/each}
					</div>
				</li>
			{/each}
		</ul>
	{:else if links}
		<p class="rounded-2xl border border-dashed border-line p-5 text-sm text-muted">{fromDatabase ? t('links.noApps') : t('links.noDatabases')}</p>
	{/if}

	{#each pending as id (id)}
		<div class="flex flex-wrap items-center gap-3 rounded-xl bg-panel px-4 py-3 text-sm">
			<span class="flex-1">{t('links.pending', { app: id })}</span>
			<Button kind="secondary" class="!h-8 !px-3.5 text-sm" {busy} onclick={() => apply(id)}>{t('app.restart')}</Button>
		</div>
	{/each}

	{#if options.length > 0}
		<form class="flex flex-wrap items-end gap-3" onsubmit={link}>
			<div class="flex min-w-40 flex-1 flex-col gap-1.5 text-sm font-medium">
				<label for="{uid}-choice">{fromDatabase ? t('links.chooseApp') : t('links.chooseDatabase')}</label>
				<select id="{uid}-choice" class="h-10 rounded-xl border border-line bg-bg px-3 text-[15px] font-normal" bind:value={choice}>
					{#each options as o (o.id)}<option value={o.id}>{o.id}{o.engine ? ` (${engineLabel[o.engine]})` : ''}</option>{/each}
				</select>
			</div>
			<label class="flex w-40 flex-col gap-1.5 text-sm font-medium">
				{t('links.prefixLabel')}
				<input
					class="h-10 rounded-xl border border-line bg-bg px-3 text-[15px] font-normal uppercase outline-none placeholder:normal-case focus:border-muted"
					placeholder={t('links.prefixNone')}
					autocomplete="off"
					spellcheck="false"
					bind:value={prefix}
				/>
			</label>
			<Button type="submit" {busy}><Link2 size={16} />{t('links.link')}</Button>
		</form>
		<p class="-mt-2 text-sm text-muted">{t('links.prefixHint')}</p>
	{:else if links && !fromDatabase && !apps.list.some(isDatabase)}
		<a href="/new/database" class="self-start text-sm underline underline-offset-2">{t('links.createFirst')}</a>
	{/if}
	<ErrorText message={error} />
</section>
