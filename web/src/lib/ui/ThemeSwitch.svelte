<script lang="ts">
	import { t } from '$lib/i18n';

	type Theme = 'system' | 'light' | 'dark';

	function stored(): Theme {
		try {
			const v = localStorage.getItem('theme');
			return v === 'light' || v === 'dark' ? v : 'system';
		} catch {
			return 'system';
		}
	}

	let theme = $state<Theme>(stored());

	function pick(next: Theme) {
		theme = next;
		const root = document.documentElement;
		try {
			if (next === 'system') localStorage.removeItem('theme');
			else localStorage.setItem('theme', next);
		} catch {
			// Private windows may refuse storage; the choice then lasts for this page.
		}
		if (next === 'system') delete root.dataset.theme;
		else root.dataset.theme = next;
	}

	const options: Theme[] = ['system', 'light', 'dark'];
</script>

<div class="flex gap-0.5 rounded-full bg-hover p-0.5 text-xs" role="radiogroup" aria-label={t('nav.theme')}>
	{#each options as o (o)}
		<button
			role="radio"
			aria-checked={theme === o}
			class="flex-1 rounded-full px-2 py-1 transition {theme === o ? 'bg-bg text-fg shadow-sm' : 'text-muted hover:text-fg'}"
			onclick={() => pick(o)}>{t(`theme.${o}`)}</button
		>
	{/each}
</div>
