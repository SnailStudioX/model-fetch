<script>
  import { ui } from "../store.svelte.js";
  import { t } from "../i18n.js";
  import Modal from "./Modal.svelte";
  import FolderBrowser from "./FolderBrowser.svelte";

  // Per-download storage pin, mirroring the Jobs page control but scoped to
  // one request: "" follows the server default, "cache"/"local" pin the
  // layout for this download only. Local mode asks for the folder here, at
  // the moment "where will this land" is being asked.
  let { layout = $bindable(""), localDir = $bindable("") } = $props();
  const tr = (key) => t(key, ui.lang);
  let picker = $state({ open: false, path: "" });
</script>

<div class="quant-route-control" style="margin-top:6px">
  <select class="search-select" style="display:block; width:100%; max-width:320px; margin:4px 0 0 12px" aria-label={tr("models.layoutLabel")} value={layout} onchange={(event) => (layout = event.target.value)}>
    <option value="">{tr("models.layoutFollow")}</option>
    <option value="cache">{tr("settings.layoutCache")}</option>
    <option value="local">{tr("settings.layoutLocal")}</option>
  </select>
  {#if layout === "local"}
    <div class="input-with-browse" style="margin:4px 0 0 12px; max-width:320px">
      <input type="text" class="path-input" placeholder="D:\\lmstudio\\models" spellcheck="false" bind:value={localDir} aria-label={tr("settings.localDir")} />
      <button type="button" class="btn btn-secondary btn-sm" onclick={() => { picker.path = String(localDir || "").trim(); picker.open = true; }}>{tr("settings.browse")}</button>
    </div>
    {#if !String(localDir || "").trim()}
      <p class="form-hint" style="margin-top:4px">{tr("models.localDirNeeded")}</p>
    {/if}
  {/if}
</div>
<Modal title={tr("settings.folderPicker")} open={picker.open} onclose={() => (picker.open = false)}>
  <FolderBrowser path={picker.path} onpick={(picked) => { localDir = picked; picker.open = false; }} />
</Modal>
