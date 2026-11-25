<script>
  import { ui } from "../store.svelte.js";
  import { t } from "../i18n.js";
  import Modal from "./Modal.svelte";
  import FolderBrowser from "./FolderBrowser.svelte";

  // Per-download storage pin, scoped to one request: an empty folder follows
  // the server default, a picked folder pins where this download writes flat.
  // The layout itself is a server-wide (advanced) setting now, so this control
  // only pins the destination folder, at the moment "where will this land" is
  // being asked.
  let { localDir = $bindable("") } = $props();
  const tr = (key) => t(key, ui.lang);
  let picker = $state({ open: false, path: "" });
</script>

<div class="quant-route-control" style="margin-top:6px">
  <div class="input-with-browse" style="margin:4px 0 0 12px; max-width:320px">
    <input type="text" class="path-input" placeholder={tr("models.localDirPlaceholder")} spellcheck="false" bind:value={localDir} aria-label={tr("settings.localDir")} />
    <button type="button" class="btn btn-secondary btn-sm" onclick={() => { picker.path = String(localDir || "").trim(); picker.open = true; }}>{tr("settings.browse")}</button>
  </div>
</div>
<Modal title={tr("settings.folderPicker")} open={picker.open} onclose={() => (picker.open = false)}>
  <FolderBrowser path={picker.path} onpick={(picked) => { localDir = picked; picker.open = false; }} />
</Modal>
