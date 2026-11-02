<script>
  import { ui, browseDirs, pickNativeFolder } from "../store.svelte.js";
  import { t, tf } from "../i18n.js";
  import Icon from "./Icon.svelte";

  let { path = "", onpick } = $props();
  const tr = (key) => t(key, ui.lang);

  let listing = $state(null);
  let error = $state("");
  let hint = $state("");
  let busy = $state(false);
  let picking = $state(false);
  let jump = $state("");
  // Roots outlive one failed listing: after a path that cannot be read, the drive row
  // is how the user gets back to a folder that can.
  let roots = $state([]);
  // The same is true of the dialog flag: it arrives with the first listing that can
  // be read, and a later failed listing must not hide a dialog that does work.
  let nativePick = $state(false);
  const current = $derived(listing?.path || path);

  async function load(target) {
    busy = true;
    error = "";
    try {
      const result = await browseDirs(target);
      listing = result;
      if (Array.isArray(result.roots) && result.roots.length) roots = result.roots;
      if (result.native_pick) nativePick = true;
    } catch (err) {
      listing = null;
      error = err.message || tr("common.error");
    } finally {
      busy = false;
    }
  }

  // Typing a path beats clicking down five levels, and it is the only way to reach a
  // folder the listing hides. The server expands "~" and a bare drive letter, so a
  // macOS home path and a Windows "D:" both work as written.
  function applyJump() {
    const target = String(jump || "").trim();
    if (!target) return;
    jump = "";
    load(target);
  }

  // The system dialog is the shortest way to a folder that is hidden from the listing
  // or already known by name. It opens on the machine that serves the page, so a
  // remote or headless server says so instead of failing after the click.
  async function pickNative() {
    picking = true;
    error = "";
    hint = "";
    try {
      const result = await pickNativeFolder(current);
      if (result.path) {
        onpick?.(result.path);
        return;
      }
      if (result.cancelled) {
        hint = tr("settings.nativePickCancelled");
      } else {
        error = tr("settings.nativePickFailed");
      }
    } catch (err) {
      error = err.message || tr("common.error");
    } finally {
      picking = false;
    }
  }

  $effect(() => {
    load(path);
  });
</script>

<div class="folder-picker">
  <p class="form-hint" style="margin-top:0">{tr("settings.folderPickerHint")}</p>
  <div class="folder-picker-bar">
    <button class="btn btn-secondary btn-sm" type="button" disabled={busy || !listing?.parent} onclick={() => load(listing.parent)}>{tr("settings.parentFolder")}</button>
    <code class="folder-picker-path" title={current}>{current || tr("common.unavailable")}</code>
  </div>
  {#if roots.length > 1}
    <div class="folder-picker-roots">
      <span class="folder-picker-roots-label">{tr("settings.folderDrives")}</span>
      {#each roots as root (root.path)}
        <button class="folder-root-btn{current === root.path ? " active" : ""}" type="button" onclick={() => load(root.path)}>{root.name}</button>
      {/each}
    </div>
  {/if}
  <div class="folder-picker-jump">
    <input type="text" id="folderJumpPath" class="path-input" placeholder={tr("settings.folderJumpPlaceholder")} spellcheck="false" autocomplete="off" bind:value={jump} onkeydown={(event) => { if (event.key === "Enter") { event.preventDefault(); applyJump(); } }} />
    <button class="btn btn-secondary btn-sm" type="button" onclick={applyJump}>{tr("settings.folderJump")}</button>
  </div>
  {#if error}
    <p class="form-hint folder-picker-error">{error}</p>
  {:else if hint}
    <p class="form-hint">{hint}</p>
  {/if}
  {#if busy}
    <p class="form-hint">{tr("common.loading")}</p>
  {:else if !listing || listing.entries.length === 0}
    <p class="form-hint">{tr("settings.noSubfolders")}</p>
  {:else}
    <div class="folder-list">
      {#each listing.entries as entry (entry.path)}
        <button class="folder-item" type="button" onclick={() => load(entry.path)}>
          <Icon name="folder" size={18} />
          <span class="folder-item-name">{entry.name}</span>
          {#if entry.is_symlink}<span class="folder-item-tag">{tr("settings.symlink")}</span>{/if}
        </button>
      {/each}
    </div>
    {#if listing.truncated}<p class="form-hint">{tf("settings.truncatedFolders", ui.lang, { n: listing.limit })}</p>{/if}
  {/if}
  <div class="folder-picker-actions">
    {#if nativePick}
      <button class="btn btn-primary btn-sm" type="button" disabled={picking} onclick={pickNative}>{picking ? tr("settings.nativePickWaiting") : tr("settings.nativePick")}</button>
      <button class="btn btn-secondary btn-sm" type="button" onclick={() => onpick?.(current)}>{tr("settings.chooseFolder")}</button>
    {:else}
      <button class="btn btn-primary btn-sm" type="button" onclick={() => onpick?.(current)}>{tr("settings.chooseFolder")}</button>
    {/if}
  </div>
  {#if nativePick}
    <p class="form-hint folder-picker-native-hint">{tr("settings.nativePickHint")}</p>
  {/if}
</div>
