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

	let token = $state('');
	let password = $state('');
	let error = $state('');
	let busy = $state(false);

	// As with the setup link, the token comes after the #, so it never
	// reaches a server log, and leaves the address bar right away.
	function readToken() {
		token = location.hash.slice(1) || token;
		if (location.hash) history.replaceState(null, '', location.pathname);
	}

	onMount(() => {
		readToken();
		window.addEventListener('hashchange', readToken);
		return () => window.removeEventListener('hashchange', readToken);
	});

	async function submit(e: SubmitEvent) {
		e.preventDefault();
		busy = true;
		error = '';
		try {
			await api('POST', '/reset', { token, password });
			await goto('/enroll');
		} catch (err) {
			error = messageOf(err);
		} finally {
			busy = false;
		}
	}
</script>

{#if token}
	<Lead title={t('reset.title')} text={' ' + t('reset.lead')} />
	<form class="flex flex-col gap-4" onsubmit={submit}>
		<Field
			label={t('reset.password')}
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
{:else}
	<Lead title={t('reset.title')} text={' ' + t('reset.noToken')} />
{/if}
