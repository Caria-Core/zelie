<script lang="ts">
	import { t } from '$lib/i18n';
	import Lead from '$lib/ui/Lead.svelte';
</script>

<div class="max-w-xl">
	<Lead title={t('home.title')} text={' ' + t('home.lead')} />
</div>
