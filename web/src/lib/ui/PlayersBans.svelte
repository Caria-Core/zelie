<script lang="ts">
	import { untrack } from 'svelte';
	import { api } from '$lib/api';
	import { messageOf } from '$lib/errors';
	import { ago } from '$lib/format';
	import { t, type Key } from '$lib/i18n';
	import { stamp, until, type AuditEntry, type Ban } from '$lib/players';
	import { unban, view } from '$lib/players.svelte';
	import Button from './Button.svelte';
	import ErrorText from './ErrorText.svelte';

	let { app }: { app: string } = $props();
	let all = $state(false);
	let bans = $state<Ban[] | null>(null);
	let audit = $state<AuditEntry[] | null>(null);
	let error = $state('');

	$effect(() => {
		app;
		all;
		view.tick;
		untrack(async () => {
			const base = `/games/${encodeURIComponent(app)}`;
			try {
				const [b, a] = await Promise.all([api<{ bans: Ban[] }>('GET', `${base}/bans?all=${all ? 1 : 0}`), api<{ entries: AuditEntry[] }>('GET', `${base}/audit?limit=50`)]);
				bans = b.bans;
				audit = a.entries;
				error = '';
			} catch (err) {
				error = messageOf(err);
			}
		});
	});

	const actionText = (a: string) => {
		const key = `players.audit.${a}` as Key;
		return t(key) === key ? a : t(key);
	};
	const status = (b: Ban) => (b.lifted_at ? t('players.banLifted', { by: b.lifted_by ?? '', when: ago(b.lifted_at) }) : !b.active ? t('players.banExpired') : '');
</script>

<div class="flex flex-col gap-8">
	<section class="flex flex-col gap-4">
		<div class="flex flex-wrap items-center justify-between gap-2">
			<p class="text-sm text-muted">{t('players.bansLead')}</p>
			<label class="inline-flex items-center gap-2 text-sm">
				<input type="checkbox" bind:checked={all} class="size-4 accent-[var(--accent)]" />{t('players.showAllBans')}
			</label>
		</div>
		<ErrorText message={error} />
		{#if bans && bans.length === 0}
			<p class="rounded-2xl border border-line px-4 py-6 text-center text-sm text-muted">{all ? t('players.noBansAll') : t('players.noBans')}</p>
		{:else if bans}
			<ul class="flex flex-col divide-y divide-line rounded-2xl border border-line">
				{#each bans as b (b.id)}
					<li class="flex flex-wrap items-center justify-between gap-x-4 gap-y-2 px-4 py-3 {b.active ? '' : 'opacity-70'}">
						<div class="flex min-w-0 flex-col gap-0.5">
							<button class="w-fit max-w-full truncate text-left text-[15px] font-medium underline decoration-line underline-offset-2 hover:decoration-fg" onclick={() => (view.detail = b.player_id)}>{b.name || b.player_id}</button>
							<span class="text-sm break-words">{b.reason || t('players.noReason')}</span>
							<span class="text-sm text-muted" title={stamp(b.created_at)}>{t('players.banBy', { by: b.by, when: ago(b.created_at) })} · {b.expires_at ? t('players.expires', { when: until(b.expires_at) }) : t('players.permanent')}</span>
							{#if status(b)}<span class="text-sm text-muted">{status(b)}</span>{/if}
						</div>
						{#if b.active}<Button kind="secondary" class="!h-9 !px-4" onclick={() => unban(app, b)}>{t('players.unban')}</Button>{/if}
					</li>
				{/each}
			</ul>
		{/if}
	</section>
	<section class="flex flex-col gap-3">
		<div>
			<h2 class="font-medium">{t('players.activity')}</h2>
			<p class="text-sm text-muted">{t('players.activityLead')}</p>
		</div>
		{#if audit && audit.length === 0}
			<p class="text-sm text-muted">{t('players.noActivity')}</p>
		{:else if audit}
			<ul class="flex flex-col divide-y divide-line rounded-2xl border border-line">
				{#each audit as a, n (n)}
					<li class="flex flex-col gap-0.5 px-4 py-2.5 text-sm">
						<span><span class="font-medium">{a.by || 'Zelie'}</span> · {actionText(a.action)}{#if a.target}<span class="text-muted">{' · '}{a.target}</span>{/if}</span>
						{#if a.detail}<span class="break-words text-muted">{a.detail}</span>{:else if !a.by}<span class="text-muted">{t('players.audit.automatic')}</span>{/if}
						<span class="text-xs text-muted" title={stamp(a.at)}>{ago(a.at)}</span>
					</li>
				{/each}
			</ul>
		{/if}
	</section>
</div>
