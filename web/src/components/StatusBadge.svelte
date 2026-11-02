<script>
  import { ui } from "../store.svelte.js";
  import { t } from "../i18n.js";
  let { status = "" } = $props();
  const key = $derived("mirror.status." + status);
  const label = $derived(t(key, ui.lang));
</script>

<span class="badge {status}">{label === key ? status : label}</span>
