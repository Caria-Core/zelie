<script lang="ts">
	import { page } from '$app/state';
	import { t } from '$lib/i18n';
	import GameFiles from '$lib/ui/GameFiles.svelte';
	import SFTPAccess from '$lib/ui/SFTPAccess.svelte';
</script>

<div class="flex flex-col gap-10">
	<GameFiles base="/a/{page.params.app}" hint={t('files.emptyApp')} />
	<SFTPAccess id={page.params.app ?? ''} />
</div>
