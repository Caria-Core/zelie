<script lang="ts">
	import { Check, KeyRound, Link2, ShieldCheck } from '@lucide/svelte';
	import { goto } from '$app/navigation';
	import { api } from '$lib/api';
	import { apps, reload, type Engine } from '$lib/apps.svelte';
	import type { Backup } from '$lib/backups';
	import { messageOf } from '$lib/errors';
	import { t } from '$lib/i18n';
	import AppIcon from '$lib/ui/AppIcon.svelte';
	import Button from '$lib/ui/Button.svelte';
	import ErrorText from '$lib/ui/ErrorText.svelte';
	import Field from '$lib/ui/Field.svelte';
	import Lead from '$lib/ui/Lead.svelte';
	import DeletedBackups from '$lib/ui/DeletedBackups.svelte';

	type EngineInfo = { name: Engine; label: string; versions: string[]; port: number; vars: string[] };

	let engines = $state<EngineInfo[]>([]);
	let engine = $state<Engine>('postgres');
	let version = $state('');
	let id = $state('');
	let named = $state(false);
	let error = $state('');
	let busy = $state(false);

	$effect(() => {
		api<EngineInfo[]>('GET', '/databases/engines')
			.then((list) => (engines = list))
			.catch((err) => (error = messageOf(err)));
	});
	const chosen = $derived(engines.find((e) => e.name === engine));

	$effect(() => {
		if (chosen && !chosen.versions.includes(version)) version = chosen.versions[0];
	});

	// Named after the engine, or db for Postgres, until the user types one.
	$effect(() => {
		if (named) return;
		const base = engine === 'postgres' ? 'db' : engine;
		const taken = new Set(apps.list.map((a) => a.id));
		let n = 1;
		while (taken.has(n === 1 ? base : `${base}-${n}`)) n++;
		id = n === 1 ? base : `${base}-${n}`;
	});

	const leads: Record<Engine, string> = {
		postgres: t('db.postgresLead'),
		mariadb: t('db.mariadbLead'),
		redis: t('db.redisLead')
	};

	function takeBack(b: Backup) {
		named = true;
		id = b.app;
		if (b.engine) engine = b.engine;
	}

	async function submit(e: SubmitEvent) {
		e.preventDefault();
		busy = true;
		error = '';
		// The list reloads below, which must not rename it mid-way.
		named = true;
		const name = id;
		try {
			await api('POST', '/databases', { id: name, engine, version });
			await reload();
			await goto('/a/' + name);
		} catch (err) {
			error = messageOf(err);
		} finally {
			busy = false;
		}
	}
</script>

<div class="flex max-w-2xl flex-col gap-8">
	<div class="flex items-center gap-4">
		<AppIcon source="image" database size="lg" />
		<Lead title={t('db.newTitle')} text={' ' + t('db.newLead')} />
	</div>
	<form class="flex flex-col gap-6" onsubmit={submit}>
		<fieldset class="flex flex-col gap-2">
			<legend class="mb-1.5 text-sm font-medium">{t('db.engine')}</legend>
			<div class="grid gap-3 sm:grid-cols-3">
				{#each engines as e (e.name)}
					<label
						class="relative flex cursor-pointer flex-col gap-1 rounded-2xl border p-4 transition has-[:focus-visible]:ring-2 has-[:focus-visible]:ring-muted {engine ===
						e.name
							? 'border-fg bg-selected'
							: 'border-line hover:bg-hover'}"
					>
						<input type="radio" name="engine" class="sr-only" value={e.name} checked={engine === e.name} onchange={() => (engine = e.name)} />
						<span class="flex items-center justify-between font-medium"
							>{e.label}{#if engine === e.name}<Check size={16} />{/if}</span
						>
						<span class="text-sm text-muted">{leads[e.name]}</span>
					</label>
				{/each}
			</div>
		</fieldset>

		{#if chosen}
			<fieldset class="flex flex-col gap-2">
				<legend class="mb-1.5 text-sm font-medium">{t('db.version')}</legend>
				<div class="flex flex-wrap gap-2">
					{#each chosen.versions as v, i (v)}
						<label
							class="cursor-pointer rounded-xl border px-3.5 py-2 text-[15px] transition has-[:focus-visible]:ring-2 has-[:focus-visible]:ring-muted {version ===
							v
								? 'border-fg bg-selected'
								: 'border-line hover:bg-hover'}"
						>
							<input type="radio" name="version" class="sr-only" value={v} bind:group={version} />
							{chosen.label}
							{v}{#if i === 0}<span class="ml-1.5 text-sm text-muted">{t('db.newest')}</span>{/if}
						</label>
					{/each}
				</div>
				<p class="text-sm text-muted">{t('db.versionHint')}</p>
			</fieldset>
		{/if}

		<Field
			label={t('new.name')}
			hint={t('db.nameHint')}
			pattern="[a-z0-9][a-z0-9\-]*"
			maxlength={40}
			required
			autocomplete="off"
			bind:value={id}
			oninput={() => (named = true)}
		/>

		<ul class="flex flex-col gap-3 rounded-2xl bg-panel p-5 text-sm">
			<li class="flex gap-3">
				<ShieldCheck size={18} strokeWidth={1.75} class="mt-0.5 shrink-0" />
				<span><span class="font-medium">{t('db.privateTitle')}</span> <span class="text-muted">{t('db.private')}</span></span>
			</li>
			<li class="flex gap-3">
				<KeyRound size={18} strokeWidth={1.75} class="mt-0.5 shrink-0" />
				<span><span class="font-medium">{t('db.passwordTitle')}</span> <span class="text-muted">{t('db.password')}</span></span>
			</li>
			<li class="flex gap-3">
				<Link2 size={18} strokeWidth={1.75} class="mt-0.5 shrink-0" />
				<span
					><span class="font-medium">{t('db.linkTitle')}</span>
					<span class="text-muted">{t('db.link', { vars: chosen ? chosen.vars.slice(0, 2).join(', ') : 'DATABASE_URL' })}</span></span
				>
			</li>
		</ul>

		<ErrorText message={error} />
		<div class="flex items-center gap-3">
			<Button type="submit" {busy} disabled={!chosen}>{t('db.create')}</Button>
			<Button kind="quiet" type="button" onclick={() => goto('/new')}>{t('new.back')}</Button>
		</div>
	</form>

	<DeletedBackups kind="database" onuse={takeBack} />
</div>
