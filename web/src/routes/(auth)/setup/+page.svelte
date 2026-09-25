<script lang="ts">
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { api } from '$lib/api';
	import { messageOf } from '$lib/errors';
	import { t } from '$lib/i18n';
	import Button from '$lib/ui/Button.svelte';
	import ErrorText from '$lib/ui/ErrorText.svelte';
	import Field from '$lib/ui/Field.svelte';
	import Lead from '$lib/ui/Lead.svelte';

	let token = '';
	let view = $state<'loading' | 'form' | 'no-token' | 'done'>('loading');
	let email = $state('');
	let password = $state('');
	let error = $state('');
	let busy = $state(false);

	// The token travels after the #, so it is never sent to the server in a
	// URL or kept in its logs. Take it out of the address bar too.
	async function readToken() {
		token = location.hash.slice(1) || token;
		if (location.hash) history.replaceState(null, '', location.pathname);
		const { open } = await api<{ open: boolean }>('GET', '/setup');
		view = !open ? 'done' : token ? 'form' : 'no-token';
	}

	onMount(() => {
		readToken();
		// Pasting the link into a tab that already shows this page only
		// changes the part after the #, which does not reload it.
		window.addEventListener('hashchange', readToken);
		return () => window.removeEventListener('hashchange', readToken);
	});

	async function submit(e: SubmitEvent) {
		e.preventDefault();
		busy = true;
		error = '';
		try {
			await api('POST', '/setup', { token, email, password });
			await goto('/enroll');
		} catch (err) {
			error = messageOf(err);
		} finally {
			busy = false;
		}
	}
</script>

{#if view === 'form'}
	<Lead title={t('setup.title')} text={' ' + t('setup.lead')} />
	<form class="flex flex-col gap-4" onsubmit={submit}>
		<Field label={t('common.email')} type="email" autocomplete="username" required bind:value={email} />
		<Field
			label={t('common.password')}
			type="password"
			autocomplete="new-password"
			minlength={10}
			required
			hint={t('setup.passwordHint')}
			bind:value={password}
		/>
		<ErrorText message={error} />
		<Button type="submit" {busy} class="mt-2 self-start">{t('common.continue')}</Button>
	</form>
{:else if view === 'no-token'}
	<Lead title={t('setup.title')} text={' ' + t('setup.noToken')} />
{:else if view === 'done'}
	<Lead title={t('setup.done')} />
	<a href="/login" class="text-muted underline-offset-4 hover:text-fg hover:underline">{t('login.submit')}</a>
{/if}
