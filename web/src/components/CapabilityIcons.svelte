<script>
  import Icon from "./Icon.svelte";
  import { ui } from "../store.svelte.js";
  import { t } from "../i18n.js";
  import { capabilityLabels } from "../capabilities.js";
  let { capabilities = [], titles = {} } = $props();
</script>

{#if capabilities.length}
  <span class="capability-icons">
    {#each capabilities as kind (kind)}
      <span class="capability-icon capability-{kind}" title={t(titles[kind], ui.lang) || t("capability." + kind, ui.lang) || capabilityLabels[kind] || kind} aria-label={t("capability." + kind, ui.lang) || capabilityLabels[kind] || kind}>
        <Icon name={kind} size={13} />
      </span>
    {/each}
  </span>
{/if}
