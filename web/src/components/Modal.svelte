<script>
  import { ui } from "../store.svelte.js";
  import { t } from "../i18n.js";

  let { title = "", open = false, onclose, children, footer } = $props();

  function onKeydown(event) {
    if (event.key === "Escape" && open) onclose?.();
  }
</script>

<svelte:window onkeydown={onKeydown} />

{#if open}
  <div class="modal-backdrop active" onclick={(event) => { if (event.target === event.currentTarget) onclose?.(); }}>
    <div class="modal" role="dialog" aria-modal="true" aria-label={title}>
      <div class="modal-header">
        <h3>{title}</h3>
        <button class="modal-close" type="button" aria-label={t("common.closeModal", ui.lang)} onclick={() => onclose?.()}>&times;</button>
      </div>
      <div class="modal-body">{@render children()}</div>
      {#if footer}
        <div class="modal-footer">{@render footer()}</div>
      {/if}
    </div>
  </div>
{/if}
