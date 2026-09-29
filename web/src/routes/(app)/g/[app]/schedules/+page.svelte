<script lang="ts">
	import { CalendarClock, ChevronDown, ChevronUp, CircleAlert, LoaderCircle, Pencil, Play, Plus, Trash, X } from '@lucide/svelte';
	import { untrack } from 'svelte';
	import { page } from '$app/state';
	import { api } from '$lib/api';
	import { ask } from '$lib/ask.svelte';
	import { messageOf } from '$lib/errors';
	import { date } from '$lib/format';
	import { say, t, type Key } from '$lib/i18n';
	import {
		emptyTask,
		maxDelay,
		maxTasks,
		powerActions,
		presetOf,
		presets,
		type PresetId,
		type Schedule,
		type ScheduleForm,
		type Schedules,
		type Task
	} from '$lib/schedules';
	import Button from '$lib/ui/Button.svelte';
	import ErrorText from '$lib/ui/ErrorText.svelte';
	import Field from '$lib/ui/Field.svelte';

	const id = $derived(page.params.app ?? '');
	let data = $state<Schedules | null>(null);
	let error = $state('');
	let busy = $state(false);

	async function refresh(server: string) {
		try {
			const d = await api<Schedules>('GET', `/games/${encodeURIComponent(server)}/schedules`);
			if (server === id) data = d;
		} catch (err) {
			error = messageOf(err);
		}
	}

	$effect(() => {
		const server = id;
		untrack(() => {
			data = null;
			refresh(server);
		});
		// Quicker while a run is going, so its result shows when it ends.
		let timer: ReturnType<typeof setTimeout>;
		const tick = () => {
			timer = setTimeout(
				async () => {
					if (document.visibilityState === 'visible') await refresh(server);
					tick();
				},
				untrack(() => data?.schedules.some((s) => s.running)) ? 2000 : 15000
			);
		};
		tick();
		return () => clearTimeout(timer);
	});

	async function act(fn: () => Promise<unknown>) {
		busy = true;
		error = '';
		try {
			await fn();
		} catch (err) {
			error = messageOf(err);
		} finally {
			busy = false;
			await refresh(id);
		}
	}

	const base = $derived(`/games/${encodeURIComponent(id)}/schedules`);

	async function runNow(s: Schedule) {
		const ok = await ask({ title: t('schedules.runNowTitle', { name: s.name }), text: t('schedules.runNowText'), action: t('schedules.runNow'), danger: false });
		if (ok) act(() => api('POST', `${base}/${s.id}/run`));
	}

	async function remove(s: Schedule) {
		const ok = await ask({ title: t('schedules.deleteConfirm', { name: s.name }), text: t('schedules.deleteConfirmText'), action: t('schedules.delete'), danger: true });
		if (ok) act(() => api('DELETE', `${base}/${s.id}`));
	}

	// The editor.
	let dialog = $state<HTMLDialogElement>();
	let editing = $state<number | null>(null);
	let form = $state<ScheduleForm>(blank());
	let preset = $state<PresetId>('daily');
	let formError = $state('');

	function blank(): ScheduleForm {
		return { name: '', cron: '0 4 * * *', only_running: false, enabled: true, tasks: [{ ...emptyTask(), action: 'power', data: 'restart' }] };
	}

	function openNew() {
		editing = null;
		form = blank();
		preset = 'daily';
		formError = '';
		dialog?.showModal();
	}

	function openEdit(s: Schedule) {
		editing = s.id;
		form = { name: s.name, cron: s.cron, only_running: s.only_running, enabled: s.enabled, tasks: s.tasks.map((x) => ({ ...x })) };
		preset = presetOf(s.cron);
		formError = '';
		dialog?.showModal();
	}

	function pickPreset() {
		const p = presets.find((x) => x.id === preset);
		if (p) form.cron = p.cron;
	}

	function move(i: number, by: -1 | 1) {
		const j = i + by;
		if (j < 0 || j >= form.tasks.length) return;
		[form.tasks[i], form.tasks[j]] = [form.tasks[j], form.tasks[i]];
	}

	function actionChanged(task: Task) {
		task.data = task.action === 'power' ? 'restart' : '';
	}

	async function save(e: SubmitEvent) {
		e.preventDefault();
		busy = true;
		formError = '';
		try {
			if (editing === null) await api('POST', base, form);
			else await api('PUT', `${base}/${editing}`, form);
			dialog?.close();
			await refresh(id);
		} catch (err) {
			formError = messageOf(err);
		} finally {
			busy = false;
		}
	}

	const time = (iso: string) => new Date(iso).toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' });
	const when = (iso: string) => `${date(iso)} ${time(iso)}`;
	const label = (s: Schedule): string => {
		const p = presetOf(s.cron);
		return p === 'custom' ? s.cron : t(`schedules.preset.${p}` as Key);
	};
	const summary = (task: Task): string =>
		task.action === 'backup'
			? t('schedules.taskSummary.backup')
			: task.action === 'power'
				? t('schedules.taskSummary.power', { data: t(`schedules.power.${task.data}` as Key) })
				: t('schedules.taskSummary.command', { data: task.data });
	const input = 'h-10 w-full min-w-0 rounded-xl border border-line bg-bg px-3 text-[15px] outline-none transition focus:border-muted';
	const badge: Record<string, string> = {
		running: 'bg-selected text-muted',
		done: 'bg-ok/10 text-ok',
		failed: 'bg-danger/10 text-danger',
		skipped: 'bg-selected text-muted'
	};
</script>

<div class="flex max-w-3xl flex-col gap-6">
	<div class="flex flex-wrap items-start justify-between gap-4">
		<div class="max-w-xl">
			<h2 class="font-medium">{t('schedules.title')}</h2>
			<p class="text-sm text-muted">{t('schedules.lead', { zone: data?.time_zone ?? '' })}</p>
		</div>
		<Button onclick={openNew} disabled={!data}><Plus size={16} strokeWidth={1.75} />{t('schedules.new')}</Button>
	</div>

	<ErrorText message={error} />

	{#if data && data.schedules.length === 0}
		<div class="flex flex-col items-start gap-3 rounded-2xl border border-dashed border-line p-5">
			<span class="grid size-10 place-items-center rounded-xl bg-selected"><CalendarClock size={20} strokeWidth={1.75} /></span>
			<p class="text-[15px]">{t('schedules.empty')}</p>
			<p class="text-sm text-muted">{t('schedules.emptyHint')}</p>
		</div>
	{:else if data}
		<ul class="flex flex-col gap-3">
			{#each data.schedules as s (s.id)}
				{@const state = s.running ? 'running' : s.last_state}
				<li class="flex flex-col gap-3 rounded-2xl border border-line p-4">
					<div class="flex flex-wrap items-start justify-between gap-3">
						<div class="min-w-0">
							<p class="flex flex-wrap items-center gap-x-2 gap-y-1">
								<span class="text-[15px] font-medium break-words">{s.name}</span>
								{#if !s.enabled}<span class="rounded-md bg-selected px-1.5 py-0.5 text-xs text-muted">{t('schedules.off')}</span>{/if}
								{#if s.only_running}<span class="rounded-md bg-selected px-1.5 py-0.5 text-xs text-muted">{t('schedules.onlyRunningTag')}</span>{/if}
							</p>
							<p class="text-sm text-muted">
								<span class={presetOf(s.cron) === 'custom' ? 'font-mono text-[13px]' : ''}>{label(s)}</span>
								· {s.next_run_at ? t('schedules.next', { when: when(s.next_run_at) }) : t('schedules.nextNone')}
							</p>
						</div>
						<div class="flex shrink-0 items-center gap-1">
							<Button kind="secondary" class="!h-8 !px-3 text-sm" disabled={busy || s.running} onclick={() => runNow(s)}
								><Play size={14} strokeWidth={1.75} />{t('schedules.runNow')}</Button
							>
							<Button kind="quiet" class="!size-8 !px-0" disabled={busy} title={t('schedules.edit')} aria-label={t('schedules.edit')} onclick={() => openEdit(s)}
								><Pencil size={15} /></Button
							>
							<Button kind="quiet" class="!size-8 !px-0 hover:!text-danger" disabled={busy} title={t('schedules.delete')} aria-label={t('schedules.delete')} onclick={() => remove(s)}
								><Trash size={15} /></Button
							>
						</div>
					</div>
					<ol class="flex flex-col gap-1 text-sm">
						{#each s.tasks as task, i (i)}
							<li class="flex gap-2.5">
								<span class="grid size-5 shrink-0 place-items-center rounded-full bg-selected text-xs">{i + 1}</span>
								<span class="min-w-0 break-words {task.action === 'command' ? 'font-mono text-[13px]' : ''}"
									>{summary(task)}{#if task.delay}<span class="font-sans text-muted"> · {t('schedules.taskDelay', { n: task.delay })}</span>{/if}</span
								>
							</li>
						{/each}
					</ol>
					<p class="flex flex-wrap items-center gap-x-2 gap-y-1 text-sm text-muted">
						{#if state}
							<span class="inline-flex items-center gap-1 rounded-md px-1.5 py-0.5 text-xs {badge[state]}">
								{#if state === 'running'}<LoaderCircle size={12} class="animate-spin" />{:else if state === 'failed'}<CircleAlert size={12} />{/if}
								{t(`schedules.state.${state}` as Key)}
							</span>
						{/if}
						{#if s.last_run_at}{t('schedules.last', { when: when(s.last_run_at) })}{:else}{t('schedules.neverRun')}{/if}
						{#if s.last_state === 'skipped' && !s.running}<span>· {t('schedules.skippedHint')}</span>{/if}
					</p>
					{#if s.last_state === 'failed' && s.last_error && !s.running}
						<p class="text-sm break-words text-danger">{t('schedules.failed', { why: say(s.last_error) })}</p>
					{/if}
				</li>
			{/each}
		</ul>
	{/if}
</div>

<dialog
	bind:this={dialog}
	class="m-auto max-h-[calc(100dvh-2rem)] w-[min(38rem,calc(100vw-2rem))] rounded-2xl border border-line bg-bg p-6 text-fg backdrop:bg-black/40"
>
	<form class="flex flex-col gap-5" onsubmit={save}>
		<h2 class="text-[17px] font-medium">{editing === null ? t('schedules.newTitle') : t('schedules.editTitle')}</h2>
		<Field label={t('schedules.name')} bind:value={form.name} placeholder={t('schedules.namePlaceholder')} maxlength={64} required />

		<div class="flex flex-col gap-3">
			<label class="flex flex-col gap-1.5 text-sm font-medium">
				{t('schedules.when')}
				<select class={input} bind:value={preset} onchange={pickPreset}>
					{#each presets as p (p.id)}<option value={p.id}>{t(`schedules.preset.${p.id}` as Key)}</option>{/each}
					<option value="custom">{t('schedules.preset.custom')}</option>
				</select>
			</label>
			{#if preset === 'custom'}
				<Field label={t('schedules.cron')} bind:value={form.cron} spellcheck={false} autocomplete="off" hint={t('schedules.cronHint')} required />
			{/if}
		</div>

		<div class="flex flex-col gap-3">
			<label class="flex items-center gap-2.5 text-[15px]">
				<input type="checkbox" class="size-4 accent-[var(--fg)]" bind:checked={form.enabled} />
				{t('schedules.enabled')}
			</label>
			<label class="flex items-start gap-2.5 text-[15px]">
				<input type="checkbox" class="mt-1 size-4 accent-[var(--fg)]" bind:checked={form.only_running} />
				<span>{t('schedules.onlyRunning')}<span class="block text-sm text-muted">{t('schedules.onlyRunningHint')}</span></span>
			</label>
		</div>

		<div class="flex flex-col gap-3">
			<div>
				<h3 class="text-sm font-medium">{t('schedules.tasks')}</h3>
				<p class="text-sm text-muted">{t('schedules.tasksHint')}</p>
			</div>
			<ul class="flex flex-col gap-3">
				{#each form.tasks as task, i (i)}
					<li class="flex flex-col gap-3 rounded-xl bg-panel p-3.5">
						<div class="flex items-center justify-between gap-2">
							<span class="text-sm font-medium">{t('schedules.task', { n: i + 1 })}</span>
							<span class="flex items-center gap-0.5">
								<Button type="button" kind="quiet" class="!size-8 !px-0" disabled={i === 0} title={t('schedules.moveUp')} aria-label={t('schedules.moveUp')} onclick={() => move(i, -1)}
									><ChevronUp size={16} /></Button
								>
								<Button
									type="button"
									kind="quiet"
									class="!size-8 !px-0"
									disabled={i === form.tasks.length - 1}
									title={t('schedules.moveDown')}
									aria-label={t('schedules.moveDown')}
									onclick={() => move(i, 1)}><ChevronDown size={16} /></Button
								>
								<Button
									type="button"
									kind="quiet"
									class="!size-8 !px-0 hover:!text-danger"
									disabled={form.tasks.length === 1}
									title={t('schedules.removeTask')}
									aria-label={t('schedules.removeTask')}
									onclick={() => form.tasks.splice(i, 1)}><X size={16} /></Button
								>
							</span>
						</div>
						<select class={input} bind:value={task.action} onchange={() => actionChanged(task)} aria-label={t('schedules.task', { n: i + 1 })}>
							{#each ['command', 'power', 'backup'] as const as a (a)}<option value={a}>{t(`schedules.action.${a}` as Key)}</option>{/each}
						</select>
						{#if task.action === 'command'}
							<Field label={t('schedules.taskCommand')} bind:value={task.data} placeholder={t('schedules.commandPlaceholder')} maxlength={1024} spellcheck={false} autocomplete="off" required />
						{:else if task.action === 'power'}
							<label class="flex flex-col gap-1.5 text-sm font-medium">
								{t('schedules.taskPower')}
								<select class={input} bind:value={task.data}>
									{#each powerActions as a (a)}<option value={a}>{t(`schedules.power.${a}` as Key)}</option>{/each}
								</select>
								{#if task.data === 'stop' || task.data === 'kill'}<span class="text-sm font-normal text-muted">{t('schedules.powerHint')}</span>{/if}
							</label>
						{/if}
						<div class="flex flex-wrap items-end gap-x-6 gap-y-3">
							<label class="flex flex-col gap-1.5 text-sm font-medium">
								{t('schedules.delay')}
								<input type="number" class="{input} !w-28" min="0" max={maxDelay} step="1" bind:value={task.delay} required />
							</label>
							<label class="flex items-center gap-2.5 pb-2 text-[15px]">
								<input type="checkbox" class="size-4 accent-[var(--fg)]" bind:checked={task.continue} />
								{t('schedules.continue')}
							</label>
						</div>
					</li>
				{/each}
			</ul>
			<Button type="button" kind="secondary" class="self-start" disabled={form.tasks.length >= maxTasks} onclick={() => form.tasks.push(emptyTask())}
				><Plus size={16} strokeWidth={1.75} />{t('schedules.addTask')}</Button
			>
		</div>

		<ErrorText message={formError} />
		<div class="flex flex-wrap gap-3">
			<Button type="submit" {busy}>{t('schedules.save')}</Button>
			<Button type="button" kind="quiet" onclick={() => dialog?.close()}>{t('common.cancel')}</Button>
		</div>
	</form>
</dialog>
