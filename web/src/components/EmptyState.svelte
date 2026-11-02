<script>
  import { ui } from "../store.svelte.js";
  import { t } from "../i18n.js";
  import Icon from "./Icon.svelte";
  let { title = "common.emptyState", hint = "", icon = "info" } = $props();
  const tr = (key) => t(key, ui.lang);
</script>

<div class="empty-state">
  <div class="empty-icon"><Icon name={icon} size={64} /></div>
  <h3>{tr(title)}</h3>
  {#if hint}<p>{tr(hint)}</p>{/if}
</div>