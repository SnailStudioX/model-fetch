<script>
  import { ui } from "../store.svelte.js";
</script>

<div class="toast-container" id="toastContainer" role="status" aria-live="polite">
  {#each ui.toasts as toast (toast.id)}
    <div class="toast {toast.kind}{toast.leaving ? " toast-leaving" : ""}">
      <span class="toast-message">{toast.message}</span>
    </div>
  {/each}
</div>