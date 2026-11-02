<script>
  import { onDestroy } from "svelte";
  import { ui, loadMirrorTargets, addMirrorTarget, removeMirrorTarget, runMirrorDiff, runMirrorSync, loadMirrorTask, cancelMirrorTask, loadCache, toast } from "../store.svelte.js";
  import { t, tf } from "../i18n.js";
  import { getTargetIcon } from "../format.js";
  import Icon from "../components/Icon.svelte";
  import Modal from "../components/Modal.svelte";
  import FolderBrowser from "../components/FolderBrowser.svelte";

  const tr = (key) => t(key, ui.lang);
  const typeLabel = (type) => (type === "model" ? tr("cache.filter.model") : tr("cache.filter.dataset"));
  // The API keeps a human-readable message for compatibility, so the notice is
  // built from the structured counts instead of the server's English text.
  // The counts are kept separate from the "nothing changed" line: a sync that
  // stopped has no such line to say, which is a different thing from finding
  // nothing to do.
  const syncChanges = (result, mode) => {
    const pull = mode === "pull";
    const parts = [];
    if (result.copied > 0) {
      const key = pull ? (result.dryRun ? "mirror.toastWouldPull" : "mirror.toastPulled") : (result.dryRun ? "mirror.toastWouldPush" : "mirror.toastPushed");
      parts.push(tf(key, ui.lang, { n: result.copied, size: result.copiedSizeHuman || "-" }));
    }
    if (result.deleted > 0) parts.push(tf("mirror.toastDeleted", ui.lang, { n: result.deleted, size: result.deletedSizeHuman || "-" }));
    return parts;
  };
  const syncMessage = (result, mode) => {
    const parts = syncChanges(result, mode);
    return parts.length ? parts.join("") : tf("mirror.toastNothing", ui.lang);
  };
  // The counts alone can read "nothing to do" while the server could not read the
  // destination at all, so the errors a result carries go into the same notice.
  const syncOutcome = (result, mode) => {
    const errors = result.errors || [];
    const message = syncMessage(result, mode) + (errors.length ? " · " + errors.join(" · ") : "");
    return { message, kind: errors.length || result.success === false ? "warning" : "success" };
  };

  const task = $derived(ui.mirror.task);
  const taskRunning = $derived(task?.state === "running");
  const taskPercent = $derived(task?.totalRepos > 0 ? Math.round((task.doneRepos / task.totalRepos) * 100) : 0);

  let showAdd = $state(false);
  let form = $state({ name: "", path: "", description: "" });
  let picker = $state({ open: false, path: "", apply: null });

  // A browser cannot return an absolute local path, so the picker lists folders
  // through the server and applies the picked path to one field.
  function openPicker(start, apply) {
    picker.open = true;
    picker.path = String(start || "").trim();
    picker.apply = apply;
  }

  const targets = $derived(ui.mirror.targets || []);
  const online = $derived(targets.filter((target) => target.exists).length);
  const summary = $derived(ui.mirror.diff?.summary || null);
  const localStats = $derived(ui.cache.stats);
  const diffs = $derived.by(() => {
    let items = ui.mirror.diff?.diffs || [];
    if (ui.mirror.diffFilter !== "all") items = items.filter((item) => item.status === ui.mirror.diffFilter);
    const query = ui.mirror.search.trim().toLowerCase();
    if (query) items = items.filter((item) => String(item.repo || "").toLowerCase().includes(query));
    return items;
  });

  $effect(() => {
    loadMirrorTargets().catch((err) => toast(err.message, "error"));
    loadCache().catch(() => {});
    // A sync outlives the request that started it, so the page has to ask what is
    // running: otherwise a reload during a multi-minute copy shows idle controls
    // and no progress, and the outcome of the one that finished is never seen.
    loadMirrorTask().catch(() => {});
  });

  // The socket carries task updates, but a page whose socket is down still has to
  // follow the sync, so while a task is running it is polled as well.
  let pollTimer = null;
  $effect(() => {
    if (!taskRunning) return;
    pollTimer = setTimeout(() => {
      loadMirrorTask().catch(() => {});
    }, 1500);
    return () => {
      if (pollTimer) clearTimeout(pollTimer);
      pollTimer = null;
    };
  });
  onDestroy(() => {
    if (pollTimer) clearTimeout(pollTimer);
    pollTimer = null;
  });

  // One notice per finished task, whether it ended here or in another tab.
  let announcedTaskId = null;
  $effect(() => {
    const current = task;
    if (!current || current.state === "running" || announcedTaskId === current.id) return;
    announcedTaskId = current.id;
    if (current.state === "cancelled") {
      // "Cancelled" by itself hides what the user can still act on: the
      // repositories that did reach the destination, and with force the one the
      // destination already had that an interrupted re-copy took with it. The
      // server reports that second part in result.errors.
      const result = current.result;
      const parts = [tr("mirror.taskCancelled"), ...(result ? [...syncChanges(result, current.mode), ...(result.errors || [])] : [])];
      toast(parts.join(" · "), "warning");
      return;
    }
    if (current.result) {
      const outcome = syncOutcome(current.result, current.mode);
      toast(outcome.message, current.state === "failed" ? "error" : outcome.kind);
      return;
    }
    toast(`${tr("common.failed")}: ${current.error || tr("common.error")}`, "error");
  });

  async function addTarget() {
    try {
      await addMirrorTarget(form);
      await loadMirrorTargets();
      showAdd = false;
      form = { name: "", path: "", description: "" };
    } catch (err) {
      toast(err.message || tr("common.error"), "error");
    }
  }

  async function removeTarget(name) {
    try {
      await removeMirrorTarget(name);
      await loadMirrorTargets();
    } catch (err) {
      toast(err.message || tr("common.error"), "error");
    }
  }

  async function diff() {
    try {
      await runMirrorDiff({ target: ui.mirror.target, repoFilter: ui.mirror.filter });
    } catch (err) {
      toast(err.message || tr("common.error"), "error");
    }
  }

  // A write sync returns a task immediately; the progress panel and the notice on
  // completion are driven by the task, not by this call.
  async function sync(mode) {
    try {
      await runMirrorSync(mode, { target: ui.mirror.target, repoFilter: ui.mirror.filter, verify: ui.mirror.verify, force: ui.mirror.force, deleteExtra: ui.mirror.deleteExtra });
    } catch (err) {
      toast(err.message || tr("common.error"), "error");
    }
  }

  function cancelTask() {
    if (!task) return;
    cancelMirrorTask(task.id).catch((err) => toast(err.message || tr("common.error"), "error"));
  }
</script>

<header class="page-header">
  <div>
    <h1>{tr("nav.mirror")}</h1>
    <p class="page-subtitle">{tr("mirror.subtitle")}</p>
  </div>
  <div class="page-actions">
    <button class="btn btn-secondary" type="button" title={tr("mirror.refreshHint")} disabled={ui.mirror.busy} onclick={() => loadMirrorTargets().catch((err) => toast(err.message, "error"))}>
      <Icon name="refresh" size={18} /> {tr("common.refresh")}
    </button>
    <button class="btn btn-primary" type="button" onclick={() => (showAdd = true)}>
      <Icon name="plus" size={18} /> {tr("mirror.addTarget")}
    </button>
  </div>
</header>

<div class="page-content">
  <div id="mirrorStats" class="mirror-stats">
    <div class="stat-card">
      <div class="stat-icon stat-icon-targets"><Icon name="mirror" size={24} /></div>
      <div class="stat-content"><div class="stat-value">{targets.length}</div><div class="stat-label">{tr("mirror.statTargets")}</div></div>
    </div>
    <div class="stat-card">
      <div class="stat-icon stat-icon-online"><Icon name="check" size={24} /></div>
      <div class="stat-content"><div class="stat-value">{online}/{targets.length}</div><div class="stat-label">{tr("mirror.statOnline")}</div></div>
    </div>
    <div class="stat-card">
      <div class="stat-icon stat-icon-local"><Icon name="cube" size={24} /></div>
      <div class="stat-content"><div class="stat-value">{localStats?.totalSizeHuman || "-"}</div><div class="stat-label">{tr("mirror.statLocalSize")}</div></div>
    </div>
    <div class="stat-card stat-card-status {summary ? (summary.inSync ? "in-sync" : "out-of-sync") : ""}" id="syncStatusCard">
      <div class="stat-icon stat-icon-sync"><Icon name="refresh" size={24} /></div>
      <div class="stat-content">
        <div class="stat-value">{summary ? (summary.inSync ? tr("mirror.badgeInSync") : tf("mirror.pendingCount", ui.lang, { n: summary.missing || 0 })) : tr("common.unknown")}</div>
        <div class="stat-label">{tr("mirror.statSyncStatus")}</div>
      </div>
    </div>
  </div>

  <div class="mirror-section">
    <div class="mirror-section-header">
      <h2>{tr("mirror.sectionTargets")}</h2>
      <p class="mirror-section-subtitle">{tr("mirror.sectionTargetsHint")}</p>
    </div>
    <div id="targetsList" class="targets-grid">
      {#if targets.length === 0}
        <div class="target-card target-card-add" role="button" tabindex="0" onclick={() => (showAdd = true)}>
          <div class="target-card-add-icon"><Icon name="plus" size={24} /></div>
          <span class="target-card-add-text">{tr("mirror.emptyTargets")}</span>
        </div>
      {:else}
        {#each targets as target (target.name)}
          <div class="target-card {target.exists ? "target-online" : "target-offline"}">
            <div class="target-card-header">
              <div class="target-card-title">
                <div class="target-card-icon"><Icon name={getTargetIcon(target.name)} size={20} /></div>
                <span class="target-card-name">{target.name}</span>
              </div>
              <div class="target-card-status {target.exists ? "online" : "offline"}">
                <span class="target-card-status-dot"></span>
                {target.exists ? tr("mirror.online") : tr("mirror.offline")}
              </div>
            </div>
            <div class="target-card-path">{target.path}</div>
            {#if target.description}<div class="target-card-description">{target.description}</div>{/if}
            <div class="target-card-footer">
              <div class="target-card-meta">
                {#if target.repoCount !== undefined}
                  <div class="target-card-meta-item"><Icon name="cubePlain" size={14} /> {tf("mirror.reposCount", ui.lang, { n: target.repoCount })}</div>
                {/if}
                {#if target.sizeHuman}
                  <div class="target-card-meta-item"><Icon name="folder" size={14} /> {target.sizeHuman}</div>
                {/if}
              </div>
              <div class="target-card-actions">
                <button class="btn btn-ghost btn-sm" type="button" title={tr("mirror.selectForSync")} onclick={() => (ui.mirror.target = target.name)}>
                  <Icon name="check" size={16} />
                </button>
                <button class="btn btn-ghost btn-sm" type="button" title={tr("mirror.removeTarget")} onclick={() => removeTarget(target.name)}>
                  <Icon name="trash" size={16} />
                </button>
              </div>
            </div>
          </div>
        {/each}
      {/if}
    </div>
  </div>

  <div class="mirror-section">
    <div class="mirror-section-header">
      <h2>{tr("mirror.sectionSync")}</h2>
      <p class="mirror-section-subtitle">{tr("mirror.sectionSyncHint")}</p>
    </div>
    <div class="sync-control-panel">
      <div class="sync-control-row">
        <div class="sync-target-select">
          <label for="syncTarget">{tr("mirror.targetSelect")}</label>
          <select id="syncTarget" bind:value={ui.mirror.target}>
            <option value="">{tr("mirror.selectTargetPlaceholder")}</option>
            {#each targets as target (target.name)}
              <option value={target.name}>{target.name} ({target.path}){target.exists ? "" : tr("mirror.offlineSuffix")}</option>
            {/each}
          </select>
        </div>
        <div class="sync-filter-input">
          <label for="syncFilter">{tr("mirror.filter")}</label>
          <div class="input-with-icon">
            <span class="input-icon"><Icon name="search" size={16} /></span>
            <input type="text" id="syncFilter" placeholder={tr("mirror.filterPlaceholder")} bind:value={ui.mirror.filter} />
          </div>
        </div>
      </div>
      <div class="sync-options-row">
        <label class="sync-option">
          <input type="checkbox" id="syncVerify" bind:checked={ui.mirror.verify} />
          <span class="sync-option-check"></span>
          <span class="sync-option-label"><strong>{tr("mirror.verify")}</strong><small>{tr("mirror.verifyHint")}</small></span>
        </label>
        <label class="sync-option">
          <input type="checkbox" id="syncForce" bind:checked={ui.mirror.force} />
          <span class="sync-option-check"></span>
          <span class="sync-option-label"><strong>{tr("mirror.force")}</strong><small>{tr("mirror.forceHint")}</small></span>
        </label>
        <label class="sync-option sync-option-danger">
          <input type="checkbox" id="syncDeleteExtra" bind:checked={ui.mirror.deleteExtra} />
          <span class="sync-option-check"></span>
          <span class="sync-option-label"><strong>{tr("mirror.deleteExtra")}</strong><small>{tr("mirror.deleteExtraHint")}</small></span>
        </label>
      </div>
      <div class="sync-actions-row">
        <button class="btn btn-outline btn-lg" id="mirrorDiffBtn" type="button" disabled={!ui.mirror.target || ui.mirror.busy || taskRunning} onclick={diff}>
          {#if ui.mirror.busy}<span class="spinner-sm"></span>{/if}<Icon name="info" size={20} /> {ui.mirror.busy ? tr("mirror.comparing") : tr("mirror.diff")}
        </button>
        <button class="btn btn-primary btn-lg" type="button" disabled={!ui.mirror.target || ui.mirror.busy || taskRunning} onclick={() => sync("push")}>
          <Icon name="push" size={20} /> {tr("mirror.push")}
        </button>
        <button class="btn btn-secondary btn-lg" type="button" disabled={!ui.mirror.target || ui.mirror.busy || taskRunning} onclick={() => sync("pull")}>
          <Icon name="pull" size={20} /> {tr("mirror.pull")}
        </button>
      </div>
      {#if task}
        <div class="mirror-task" id="mirrorTask" role="status" aria-live="polite">
          <div class="mirror-task-head">
            <span class="mirror-task-state">
              {task.phase === "cancelling" ? tr("mirror.taskCancelling") : tr("mirror.task." + task.state)}
            </span>
            <span class="mirror-task-count">{tf("mirror.taskProgress", ui.lang, { done: task.doneRepos, total: task.totalRepos })}</span>
            {#if taskRunning}
              <button class="btn btn-ghost btn-sm" id="mirrorTaskCancel" type="button" onclick={cancelTask}>{tr("mirror.taskCancel")}</button>
            {/if}
          </div>
          <div class="progress-bar"><div class="progress-fill" style="width: max(3px, {taskPercent}%)"></div></div>
          {#if task.currentRepo}
            <p class="form-hint">{tf("mirror.taskRepo", ui.lang, { repo: task.currentRepo })}</p>
          {/if}
          {#if task.error}
            <p class="form-hint">{task.error}</p>
          {/if}
        </div>
      {/if}
    </div>
  </div>

  {#if ui.mirror.diff}
    <div class="mirror-section" id="mirrorDiffSection">
      <div class="mirror-section-header">
        <h2>{tr("mirror.sectionDiff")}</h2>
        <div class="diff-header-actions">
          <div class="diff-summary-badges" id="diffSummary">
            {#if summary?.inSync}
              <span class="badge badge-success">{tr("mirror.badgeInSync")}</span>
            {:else}
              {#if summary?.missing}<span class="badge badge-warning">{tf("mirror.badgeToPush", ui.lang, { n: summary.missing, size: summary.missingSizeHuman })}</span>{/if}
              {#if summary?.extra}<span class="badge badge-info">{tf("mirror.badgeExtra", ui.lang, { n: summary.extra })}</span>{/if}
              {#if summary?.outdated}<span class="badge badge-secondary">{tf("mirror.badgeOutdated", ui.lang, { n: summary.outdated })}</span>{/if}
            {/if}
          </div>
        </div>
      </div>
      <div class="diff-toolbar">
        <div class="diff-search">
          <span class="search-icon"><Icon name="search" size={18} /></span>
          <input type="text" id="diffSearch" placeholder={tr("mirror.searchPlaceholder")} autocomplete="off" bind:value={ui.mirror.search} />
        </div>
        <div class="diff-filters">
          <button class="filter-btn{ui.mirror.diffFilter === "all" ? " active" : ""}" type="button" data-diff-filter="all">{tr("cache.filter.all")}</button>
          <button class="filter-btn{ui.mirror.diffFilter === "missing" ? " active" : ""}" type="button" data-diff-filter="missing"><span class="filter-dot filter-dot-warning"></span> {tr("mirror.status.missing")}</button>
          <button class="filter-btn{ui.mirror.diffFilter === "extra" ? " active" : ""}" type="button" data-diff-filter="extra"><span class="filter-dot filter-dot-info"></span> {tr("mirror.status.extra")}</button>
          <button class="filter-btn{ui.mirror.diffFilter === "outdated" ? " active" : ""}" type="button" data-diff-filter="outdated"><span class="filter-dot filter-dot-muted"></span> {tr("mirror.status.outdated")}</button>
        </div>
      </div>
      <div id="diffResults" class="diff-results">
        {#if (ui.mirror.diff?.warnings || []).length}
          <div role="status" id="mirrorDiffWarnings">
            {#each ui.mirror.diff.warnings as warning (warning)}
              <p class="form-hint">{warning}</p>
            {/each}
          </div>
        {/if}
        {#if summary?.inSync}
          <div class="empty-state" style="padding: 48px 24px;">
            <div class="empty-icon" style="color: var(--color-success);"><Icon name="check" size={64} /></div>
            <h3>{tr("mirror.inSync")}</h3>
            <p>{tr("mirror.noDiffHint")}</p>
          </div>
        {:else if diffs.length === 0}
          <div class="empty-state" style="padding: 48px 24px;">
            <h3>{tr("mirror.noMatches")}</h3>
            <p>{tr("mirror.noMatchesHint")}</p>
          </div>
        {:else}
          <div class="diff-results-header">
            <span>{tr("common.colStatus")}</span><span>{tr("common.colType")}</span><span>{tr("common.colRepo")}</span><span>{tr("common.colSize")}</span><span></span>
          </div>
          {#each diffs as item (item.repo + "|" + item.status)}
            <div class="diff-item">
              <div><span class="diff-item-status {item.status}">{tr("mirror.status." + item.status)}</span></div>
              <div class="diff-item-type"><Icon name={item.type === "model" ? "cubePlain" : "book"} size={14} /> {typeLabel(item.type)}</div>
              <div class="diff-item-repo">{item.repo}</div>
              <div class="diff-item-size">{item.sizeHuman || "-"}</div>
              <div class="diff-item-action">
                {#if item.status === "missing"}
                  <button class="btn btn-ghost btn-sm" type="button" title={tr("mirror.willBePushed")} onclick={(event) => event.stopPropagation()}>
                    <Icon name="push" size={14} />
                  </button>
                {/if}
              </div>
            </div>
          {/each}
        {/if}
      </div>
    </div>
  {/if}
</div>

<Modal title={tr("mirror.addTarget")} open={showAdd} onclose={() => (showAdd = false)}>
  <form class="modal-form" onsubmit={(event) => { event.preventDefault(); addTarget(); }}>
    <div class="form-group">
      <label for="mirror-name">{tr("mirror.name")}</label>
      <input id="mirror-name" type="text" spellcheck="false" required bind:value={form.name} />
      <p class="form-hint">{tr("mirror.nameHint")}</p>
    </div>
    <div class="form-group">
      <label for="mirror-path">{tr("mirror.path")}</label>
      <div class="input-with-browse">
        <input id="mirror-path" type="text" class="path-input" spellcheck="false" required bind:value={form.path} />
        <button type="button" class="btn btn-secondary btn-sm" onclick={() => openPicker(form.path, (picked) => (form.path = picked))}>{tr("settings.browse")}</button>
      </div>
    </div>
    <div class="form-group">
      <label for="mirror-desc">{tr("mirror.description")}</label>
      <input id="mirror-desc" type="text" spellcheck="false" bind:value={form.description} />
    </div>
    <div class="form-actions">
      <button class="btn btn-secondary" type="button" onclick={() => (showAdd = false)}>{tr("common.cancel")}</button>
      <button class="btn btn-primary" type="submit">{tr("mirror.add")}</button>
    </div>
  </form>
</Modal>

<Modal title={tr("settings.folderPicker")} open={picker.open} onclose={() => (picker.open = false)}>
  {#if picker.open}
    <FolderBrowser path={picker.path} onpick={(picked) => { picker.apply?.(picked); picker.open = false; }} />
  {/if}
</Modal>

