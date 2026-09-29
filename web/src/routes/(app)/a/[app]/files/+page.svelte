<script lang="ts">
	import { page } from '$app/state';
	import { t } from '$lib/i18n';
	import GameFiles from '$lib/ui/GameFiles.svelte';
</script>

<GameFiles base="/a/{page.params.app}" hint={t('files.emptyApp')} />
