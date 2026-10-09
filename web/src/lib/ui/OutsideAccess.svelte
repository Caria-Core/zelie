<script lang="ts">
	import { ask } from '$lib/ask.svelte';
	import { Copy, Check, KeyRound, Laptop, Server, TriangleAlert } from '@lucide/svelte';
	import { api, ApiError } from '$lib/api';
	import type { App } from '$lib/apps.svelte';
	import { sensitive } from '$lib/confirm.svelte';
	import { messageOf } from '$lib/errors';
	import { t } from '$lib/i18n';
	import Button from './Button.svelte';
	import ErrorText from './ErrorText.svelte';

	// Outside access: a desktop tool reaches the database through an SSH
	// tunnel to a port only this server itself can open. The password is
	// shown once, right after it is made. When another program holds the
	// port the server sends no way in, only the port and a free one to move to.
	type External = {
		enabled: boolean;
		port?: number;
		user?: string;
		database?: string;
		password?: string;
		port_taken?: boolean;
		free_port?: number;
	};

	let { app }: { app: App } = $props();
	// The app is replaced by a fresh copy every few seconds, and an effect
	// that read it would run each time: asking the core about the port again
	// and dropping the password that is shown once. The id only changes when
	// the page moves to another database.
	const id = $derived(app.id);
	let x = $state<External | null>(null);
	let password = $state('');
	let error = $state('');
	let busy = $state(false);
	let copied = $state('');

	// An answer for the database the page has since moved away from is dropped.
	async function load() {
		const want = id;
		const r = await api<External>('GET', `/apps/${want}/external`);
		if (want === id) x = r;
	}

	$effect(() => {
		const want = id;
		x = null;
		password = '';
		error = '';
		load().catch((err) => want === id && (error = messageOf(err)));
	});

	// The address the browser reached the panel at is the server's.
	const host = typeof location === 'undefined' ? '' : location.hostname;
	const running = $derived(app.state === 'running');
	const tunnel = $derived(x?.port ? `ssh -N -L ${x.port}:127.0.0.1:${x.port} root@${host}` : '');
	const fields = $derived(
		x?.enabled
			? [
					{ label: t('outside.sshHost'), value: host },
					{ label: t('outside.sshUser'), value: 'root' },
					{ label: t('db.host'), value: '127.0.0.1' },
					{ label: t('db.port'), value: String(x.port) },
					{ label: t('db.user'), value: x.user ?? '' },
					// Redis numbers its databases; everything is in the first.
					{ label: t('db.database'), value: x.database ?? '0' }
				]
			: []
	);

	async function act(fn: () => Promise<void>) {
		busy = true;
		error = '';
		try {
			await fn();
		} catch (err) {
			error = messageOf(err);
			// The server may know more than the page does: a port that turned
			// out to be taken takes the connection details away. The error
			// above is the one to show, so a failed read adds nothing.
			if (err instanceof ApiError && fn !== load) await load().catch(() => {});
		} finally {
			busy = false;
		}
	}

	const setUp = () =>
		act(async () => {
			const r = await sensitive(() => api<External>('POST', `/apps/${id}/external`));
			password = r.password ?? '';
			x = { ...r, password: undefined };
		});

	const move = (port: number) =>
		act(async () => {
			x = await api<External>('PUT', `/apps/${id}/external`, { port });
		});

	async function turnOff() {
		if (!(await ask({ title: t('outside.offConfirm'), text: t('outside.offConfirmText'), action: t('common.turnOff'), danger: true }))) return;
		act(async () => {
			await api('DELETE', `/apps/${id}/external`);
			x = { enabled: false };
			password = '';
		});
	}

	async function copy(what: string, text: string) {
		await navigator.clipboard.writeText(text);
		copied = what;
		setTimeout(() => copied === what && (copied = ''), 2000);
	}
</script>

{#snippet copyButton(what: string, text: string)}
	<Button kind="quiet" class="!h-8 shrink-0 !px-2" title={t('outside.copy')} aria-label={t('outside.copy')} onclick={() => copy(what, text)}>
		{#if copied === what}<Check size={16} />{:else}<Copy size={16} />{/if}
	</Button>
{/snippet}

<section class="flex flex-col gap-4">
	<div>
		<h2 class="font-medium">{t('outside.title')}</h2>
		<p class="text-sm text-muted">{t('outside.lead')}</p>
	</div>

	{#if x && !x.enabled}
		<ol class="flex flex-col gap-3 rounded-2xl border border-line p-5 text-sm">
			<li class="flex gap-3">
				<Server size={18} strokeWidth={1.75} class="mt-0.5 shrink-0" />
				<span><span class="font-medium">{t('outside.step1Title')}</span> <span class="text-muted">{t('outside.step1')}</span></span>
			</li>
			<li class="flex gap-3">
				<Laptop size={18} strokeWidth={1.75} class="mt-0.5 shrink-0" />
				<span><span class="font-medium">{t('outside.step2Title')}</span> <span class="text-muted">{t('outside.step2')}</span></span>
			</li>
			<li class="flex gap-3">
				<KeyRound size={18} strokeWidth={1.75} class="mt-0.5 shrink-0" />
				<span><span class="font-medium">{t('outside.step3Title')}</span> <span class="text-muted">{t('outside.step3')}</span></span>
			</li>
		</ol>
		<div class="flex flex-wrap items-center gap-3">
			<Button {busy} disabled={!running} onclick={setUp}>{t('outside.setUp')}</Button>
			{#if !running}<span class="text-sm text-muted">{t('outside.notRunning')}</span>{/if}
		</div>
	{:else if x?.port_taken}
		<div class="flex flex-col gap-3 rounded-2xl border border-line bg-panel p-4">
			<p class="flex gap-2.5 text-sm">
				<TriangleAlert size={16} strokeWidth={1.75} class="mt-0.5 shrink-0" />
				<span><span class="font-medium">{t('outside.takenTitle', { port: x.port ?? 0 })}</span> <span class="text-muted">{t('outside.taken')}</span></span>
			</p>
			<p class="text-sm text-muted">{x.free_port ? t('outside.moveLead', { port: x.free_port }) : t('outside.noFreePort')}</p>
		</div>
		<div class="flex flex-wrap items-center gap-3">
			{#if x.free_port}
				<Button {busy} onclick={() => move(x?.free_port ?? 0)}>{t('outside.usePort', { port: x.free_port })}</Button>
			{/if}
			<Button kind={x.free_port ? 'secondary' : 'primary'} {busy} onclick={() => act(load)}>{t('outside.checkAgain')}</Button>
			<Button kind="quiet" {busy} disabled={!running} class="hover:!text-danger" onclick={turnOff}>{t('outside.turnOff')}</Button>
		</div>
		{#if !running}<p class="text-sm text-muted">{t('outside.notRunning')}</p>{/if}
	{:else if x?.enabled}
		{#if password}
			<div class="flex flex-col gap-3 rounded-2xl border border-line bg-panel p-4">
				<p class="flex gap-2.5 text-sm"><TriangleAlert size={16} strokeWidth={1.75} class="mt-0.5 shrink-0" />{t('outside.passwordOnce')}</p>
				<div class="flex items-center gap-2 rounded-xl bg-bg px-3 py-2">
					<code class="min-w-0 flex-1 font-mono text-[15px] break-all">{password}</code>
					{@render copyButton('password', password)}
				</div>
			</div>
		{/if}

		<div class="flex flex-col gap-2">
			<h3 class="text-sm font-medium">{t('outside.tunnelTitle')}</h3>
			<div class="flex items-center gap-2 rounded-xl bg-panel px-3 py-2">
				<code class="min-w-0 flex-1 font-mono text-sm break-all">{tunnel}</code>
				{@render copyButton('tunnel', tunnel)}
			</div>
			<p class="text-sm text-muted">{t('outside.tunnelLead', { port: x.port ?? 0 })}</p>
		</div>

		<div class="flex flex-col gap-2">
			<h3 class="text-sm font-medium">{t('outside.toolTitle')}</h3>
			<dl class="grid grid-cols-2 gap-px overflow-hidden rounded-2xl border border-line bg-line sm:grid-cols-3">
				{#each fields as f (f.label)}
					<div class="flex min-w-0 flex-col gap-1 bg-bg px-4 py-3">
						<dt class="text-sm text-muted">{f.label}</dt>
						<dd class="truncate font-mono text-[15px]">{f.value}</dd>
					</div>
				{/each}
			</dl>
			<p class="text-sm text-muted">{password ? t('outside.passwordAbove') : t('outside.passwordHidden')} {t('outside.sshUserHint')}</p>
		</div>

		<div class="flex flex-wrap gap-3">
			<Button kind="secondary" {busy} disabled={!running} onclick={setUp}>{t('outside.newPassword')}</Button>
			<Button kind="quiet" {busy} disabled={!running} class="hover:!text-danger" onclick={turnOff}>{t('outside.turnOff')}</Button>
		</div>
		{#if !running}<p class="text-sm text-muted">{t('outside.notRunning')}</p>{/if}
	{/if}
	<ErrorText message={error} />
</section>
