<script lang="ts">
	import { ExternalLink, Trash, X } from '@lucide/svelte';
	import { untrack } from 'svelte';
	import { api } from '$lib/api';
	import { ask } from '$lib/ask.svelte';
	import { ApiError } from '$lib/api';
	import { messageOf } from '$lib/errors';
	import { ago, date } from '$lib/format';
	import { t, type Key } from '$lib/i18n';
	import { span, stamp, until, type PlayerDetail } from '$lib/players';
	import { changed, unban, view } from '$lib/players.svelte';
	import CopyId from './CopyId.svelte';
	import ErrorText from './ErrorText.svelte';
	import IpText from './IpText.svelte';
	import IpToggle from './IpToggle.svelte';
	import PlayerBadges from './PlayerBadges.svelte';
	import PlayerMenu from './PlayerMenu.svelte';
	import PlayerName from './PlayerName.svelte';
	import PlayersChat from './PlayersChat.svelte';
	import StateDot from './StateDot.svelte';

	let { app, minecraft }: { app: string; minecraft: boolean } = $props();
	let dialog = $state<HTMLDialogElement>();
	let data = $state<PlayerDetail | null>(null);
	let error = $state('');
	let seq = 0;

	$effect(() => {
		if (view.detail && !dialog?.open) dialog?.showModal();
		if (!view.detail && dialog?.open) dialog.close();
	});

	$effect(() => {
		const id = view.detail;
		view.tick;
		if (!id) return;
		untrack(async () => {
			const mine = ++seq;
			try {
				const d = await api<PlayerDetail>('GET', `/games/${encodeURIComponent(app)}/players/${encodeURIComponent(id)}`);
				if (mine === seq) {
					data = d;
					error = '';
				}
			} catch (err) {
				if (mine === seq) error = err instanceof ApiError && err.msg.code === 'players.not_found' ? t('players.neverJoined') : messageOf(err);
			}
		});
	});

	function close() {
		dialog?.close();
		view.detail = '';
		data = null;
		error = '';
	}

	async function removeNote(id: number) {
		if (!(await ask({ title: t('players.noteDeleteConfirm'), action: t('common.remove'), danger: true }))) return;
		try {
			await api('DELETE', `/games/${encodeURIComponent(app)}/players/${encodeURIComponent(view.detail)}/notes/${id}`);
			changed();
		} catch (err) {
			error = messageOf(err);
		}
	}

	const h = 'font-medium';
	const box = 'flex flex-col divide-y divide-line rounded-2xl border border-line';
</script>

<dialog
	bind:this={dialog}
	oncancel={(e) => (e.preventDefault(), close())}
	class="m-0 mr-0 ml-auto h-dvh max-h-none w-[min(42rem,100vw)] max-w-none border-l border-line bg-bg p-0 text-fg backdrop:bg-black/40 sm:rounded-l-2xl"
>
	{#if view.detail}
		<div class="flex flex-col gap-6 p-5 sm:p-6">
			{#if data && data.player.id === view.detail}
				{@const p = data.player}
				{@const activeBan = data.bans.find((b) => b.active)}
				<header class="flex items-start justify-between gap-3">
					<div class="flex min-w-0 flex-col gap-2">
						<PlayerName name={p.name} steam={p.steam} size="lg" />
						<div class="flex flex-wrap items-center gap-x-3 gap-y-1 text-muted">
							<CopyId text={p.id} />
							{#if p.online}<StateDot state="running" /><span class="text-sm">{t('players.onlineNow')}</span>{/if}
							{#if p.steam?.profile_url}
								<a href={p.steam.profile_url} target="_blank" rel="noopener noreferrer" class="inline-flex items-center gap-1 text-sm underline decoration-line underline-offset-2 hover:text-fg hover:decoration-fg">{t('players.steamProfile')}<ExternalLink size={13} /></a>
							{/if}
						</div>
						<div class="flex flex-wrap gap-1.5"><PlayerBadges banned={p.banned} notes={0} steam={p.steam} /></div>
					</div>
					<div class="flex shrink-0 items-center gap-1">
						<PlayerMenu id={p.id} name={p.name} banned={p.banned} online={p.online} {minecraft} profile={false} />
						<button class="rounded-lg p-2 text-muted transition hover:bg-hover hover:text-fg" aria-label={t('common.close')} onclick={close}><X size={18} strokeWidth={1.75} /></button>
					</div>
				</header>
				<ErrorText message={error} />
				{#if view.error}<ErrorText message={view.error} />{/if}
				<dl class="grid grid-cols-2 gap-px overflow-hidden rounded-2xl border border-line bg-line sm:grid-cols-3">
					<div class="flex flex-col gap-1 bg-bg px-4 py-3"><dt class="text-sm text-muted">{t('players.totalTime')}</dt><dd class="text-[15px] tabular-nums">{span(p.play_seconds)}</dd></div>
					<div class="flex flex-col gap-1 bg-bg px-4 py-3"><dt class="text-sm text-muted">{t('players.firstSeenLabel')}</dt><dd class="text-[15px]" title={stamp(p.first_seen)}>{date(p.first_seen)}</dd></div>
					<div class="col-span-2 flex flex-col gap-1 bg-bg px-4 py-3 sm:col-span-1"><dt class="text-sm text-muted">{t('players.lastSeenLabel')}</dt><dd class="text-[15px]" title={stamp(p.last_seen)}>{p.online ? t('players.onlineNow') : ago(p.last_seen)}</dd></div>
				</dl>
				{#if activeBan}
					<p class="rounded-xl border border-danger/30 px-4 py-3 text-sm text-danger">
						{t('players.bannedNote', { reason: activeBan.reason || t('players.noReason'), when: activeBan.expires_at ? until(activeBan.expires_at) : t('players.permanent') })}
					</p>
				{/if}

				<section class="flex flex-col gap-3">
					<div class="flex items-center justify-between gap-2"><h3 class={h}>{t('players.sessions')}</h3><IpToggle /></div>
					{#if data.sessions.length === 0}<p class="text-sm text-muted">{t('players.noSessions')}</p>{:else}
						<ul class={box}>
							{#each data.sessions as s, n (n)}
								<li class="flex flex-wrap items-center justify-between gap-x-4 gap-y-1 px-4 py-2.5 text-sm">
									<span title={stamp(s.joined_at)}>{ago(s.joined_at)}</span>
									<span class="text-muted tabular-nums">{s.left_at ? span((new Date(s.left_at).getTime() - new Date(s.joined_at).getTime()) / 1000) : t('players.onlineNow')}</span>
									<IpText ip={s.ip} />
								</li>
							{/each}
						</ul>
					{/if}
				</section>

				<section class="flex flex-col gap-3">
					<div>
						<h3 class={h}>{t('players.sameIp')}</h3>
						<p class="text-sm text-muted">{t('players.sameIpLead')}</p>
					</div>
					{#if data.shared_ip.length === 0}<p class="text-sm text-muted">{t('players.noSameIp')}</p>{:else}
						<ul class={box}>
							{#each data.shared_ip as o (o.id + o.ip)}
								<li class="flex flex-wrap items-center justify-between gap-x-4 gap-y-1 px-4 py-2.5 text-sm">
									<button class="min-w-0 truncate text-left font-medium underline decoration-line underline-offset-2 hover:decoration-fg" onclick={() => (view.detail = o.id)}>{o.name || o.id}</button>
									<IpText ip={o.ip} />
									<span class="text-muted" title={stamp(o.last_seen)}>{ago(o.last_seen)}</span>
								</li>
							{/each}
						</ul>
					{/if}
				</section>

				<section class="flex flex-col gap-3">
					<div class="flex items-center justify-between gap-2">
						<h3 class={h}>{t('players.notes')}</h3>
						<button class="text-sm text-muted underline decoration-line underline-offset-2 hover:text-fg hover:decoration-fg" onclick={() => (view.form = { kind: 'note', id: p.id, name: p.name })}>{t('players.addNote')}</button>
					</div>
					{#if data.notes.length === 0}<p class="text-sm text-muted">{t('players.noNotes')}</p>{:else}
						<ul class={box}>
							{#each data.notes as n (n.id)}
								<li class="flex items-start justify-between gap-3 px-4 py-2.5">
									<div class="flex min-w-0 flex-col gap-1 text-sm">
										<div class="flex flex-wrap items-center gap-2">
											{#if n.tag}<span class="rounded-full bg-selected px-2.5 py-0.5 text-xs">{t(`players.tag.${n.tag}` as Key)}</span>{/if}
											<span class="text-xs text-muted" title={stamp(n.at)}>{t('players.noteBy', { by: n.by, when: ago(n.at) })}</span>
										</div>
										<p class="break-words whitespace-pre-line">{n.note}</p>
									</div>
									<button class="rounded-lg p-1.5 text-muted transition hover:bg-hover hover:text-danger" aria-label={t('players.noteDelete')} title={t('players.noteDelete')} onclick={() => removeNote(n.id)}><Trash size={16} strokeWidth={1.75} /></button>
								</li>
							{/each}
						</ul>
					{/if}
				</section>

				<section class="flex flex-col gap-3">
					<h3 class={h}>{t('players.banHistory')}</h3>
					{#if data.bans.length === 0}<p class="text-sm text-muted">{t('players.noBanHistory')}</p>{:else}
						<ul class={box}>
							{#each data.bans as b (b.id)}
								<li class="flex flex-wrap items-center justify-between gap-x-4 gap-y-2 px-4 py-2.5 text-sm">
									<div class="flex min-w-0 flex-col gap-0.5">
										<span class="break-words">{b.reason || t('players.noReason')}</span>
										<span class="text-muted" title={stamp(b.created_at)}>{t('players.banBy', { by: b.by, when: ago(b.created_at) })} · {b.expires_at ? t('players.expires', { when: until(b.expires_at) }) : t('players.permanent')}</span>
										{#if b.lifted_at}<span class="text-muted">{t('players.banLifted', { by: b.lifted_by ?? '', when: ago(b.lifted_at) })}</span>{:else if !b.active}<span class="text-muted">{t('players.banExpired')}</span>{/if}
									</div>
									{#if b.active}<button class="rounded-full bg-selected px-3.5 py-1.5 transition hover:bg-line" onclick={() => unban(app, { id: b.id, name: p.name })}>{t('players.unban')}</button>{/if}
								</li>
							{/each}
						</ul>
					{/if}
				</section>

				<section class="flex flex-col gap-3">
					<h3 class={h}>{t('players.reportsAgainst')}</h3>
					{#if data.reports.length === 0}<p class="text-sm text-muted">{t('players.noReportsAgainst')}</p>{:else}
						<ul class={box}>
							{#each data.reports as r, n (n)}
								<li class="flex flex-col gap-0.5 px-4 py-2.5 text-sm">
									<span class="text-muted">{t('players.reportBy', { name: r.reporter_name || r.reporter_id, when: stamp(r.at) })}</span>
									{#if r.subject}<span class="font-medium">{r.subject}</span>{/if}
									{#if r.message}<span class="break-words">{r.message}</span>{/if}
								</li>
							{/each}
						</ul>
					{/if}
				</section>

				<section class="flex flex-col gap-3">
					<h3 class={h}>{t('players.recentChat')}</h3>
					{#key p.id}<PlayersChat {app} player={p.id} compact />{/key}
				</section>
			{:else}
				<div class="flex justify-end"><button class="rounded-lg p-2 text-muted transition hover:bg-hover hover:text-fg" aria-label={t('common.close')} onclick={close}><X size={18} strokeWidth={1.75} /></button></div>
				{#if error}<ErrorText message={error} />{:else}<p class="text-sm text-muted">{t('players.loading')}</p>{/if}
			{/if}
		</div>
	{/if}
</dialog>
