<script>
  import { onDestroy } from "svelte";
  import { ui, loadCache, loadCacheDetails, loadCacheSelection, deleteCacheSelection, rebuildCache, deleteCacheRepo, loadSettings, saveSettings, startCacheCheck, loadCacheCheck, cancelCacheCheck, toast } from "../store.svelte.js";
  import { t, tf } from "../i18n.js";
  import { formatBytes, meaningfulHubDir } from "../format.js";
  import { detectSearchCapabilities, mergeCapabilities } from "../capabilities.js";
  import Icon from "../components/Icon.svelte";
  import CapabilityIcons from "../components/CapabilityIcons.svelte";
  import Modal from "../components/Modal.svelte";
  import FolderBrowser from "../components/FolderBrowser.svelte";

  const tr = (key) => t(key, ui.lang);
  const typeLabel = (type) => (type === "model" ? tr("cache.filter.model") : tr("cache.filter.dataset"));
  const statusLabel = (status) => {
    if (status === "complete") return tr("cache.badgeComplete");
    if (status === "filtered") return tr("cache.badgeFiltered");
    if (status === "unknown") return tr("cache.badgeExternal");
    return tr("cache.badgeTracked");
  };

  let confirm = $state(null);
  let confirmRepo = $state(null);
  let rebuildResult = $state(null);
  let rebuilding = $state(false);

  async function runRebuild() {
    rebuilding = true;
    try {
      const result = await rebuildCache();
      rebuildResult = result;
      await loadCache();
    } catch (err) {
      toast(err.message || tr("common.error"), "error");
    } finally {
      rebuilding = false;
    }
  }

  // A check answers "is what I downloaded still what it was". The hub cache names
  // every blob by the SHA-256 of its contents, so a quick pass needs only the
  // names and sizes, and the byte pass re-reads everything and compares each file
  // with the digest it is stored under. Only the hub cache is covered: the local
  // scan folders hold files that were never content-addressed, so there is nothing
  // stored to compare their bytes against. The panel says which pass ran, because
  // "checked" would otherwise mean two different things.
  const check = $derived(ui.cache.check);
  const checkRunning = $derived(check?.state === "running");
  const checkPercent = $derived(check?.totalRepos > 0 ? Math.round((check.doneRepos / check.totalRepos) * 100) : 0);
  const checkResult = $derived(check?.result || null);
  const problemRepos = $derived(checkResult?.repos || []);
  const checkKind = (kind) => tr("cache.check.kind." + kind);

  async function startChecked(options) {
    try {
      await startCacheCheck(options);
    } catch (err) {
      toast(err.message || tr("common.error"), "error");
    }
  }

  // The storage card on the Settings page used to hold these. They belong here
  // because they answer "what is this page looking at": the Hub cache the server
  // reads, the friendly view root, and the extra local folders that are scanned
  // as if they were cached. Editing one is only useful once the listing is read
  // again, so each save is followed by a reload.
  const settings = $derived(ui.settings || {});
  let roots = $state({ cacheDir: "", scanDirs: "" });
  let picker = $state({ open: false, path: "", apply: null });

  $effect(() => {
    const loaded = ui.settings;
    if (!loaded) return;
    roots.cacheDir = loaded.configuredCacheDir ?? "";
    roots.scanDirs = (loaded.localScanDirs || []).join("\n");
  });

  // The listing is what this strip describes, so it is read on entering the page
  // rather than only after the user finds the refresh button.
  $effect(() => {
    loadCache().catch(() => {});
    // A check outlives the request that started it, so a page opened while one is
    // running has to ask what it found: otherwise it shows a library as unexamined
    // and never shows the report of the pass that already finished.
    loadCacheCheck().catch(() => {});
  });

  // The socket carries the progress frames, but a page whose socket is down still
  // has to follow the pass, so while one runs it is polled as well.
  let checkPoll = null;
  $effect(() => {
    if (!checkRunning) return;
    checkPoll = setTimeout(() => {
      loadCacheCheck().catch(() => {});
    }, 1500);
    return () => {
      if (checkPoll) clearTimeout(checkPoll);
      checkPoll = null;
    };
  });
  onDestroy(() => {
    if (checkPoll) clearTimeout(checkPoll);
    checkPoll = null;
  });

  // One notice per finished pass, whether it ended here or in another tab. The
  // text is built from the counts: the server's message is English and reads the
  // same whether the library is fine or not.
  let announcedCheckId = null;
  $effect(() => {
    const current = check;
    if (!current || current.state === "running" || announcedCheckId === current.id) return;
    announcedCheckId = current.id;
    if (current.state === "cancelled") {
      toast(tr("cache.check.toastCancelled"), "warning");
      return;
    }
    const result = current.result;
    if (!result) {
      toast(`${tr("common.failed")}: ${current.error || tr("common.error")}`, "error");
      return;
    }
    if (result.success) {
      toast(tf("cache.check.toastClean", ui.lang, { n: result.reposChecked, size: result.bytesHuman || "0 B" }), "success");
      return;
    }
    toast(tf("cache.check.toastProblems", ui.lang, { problems: result.reposProblem || 0, unreadable: result.reposUnreadable || 0 }), "warning");
  });

  async function applyCacheDir() {
    const cacheDir = String(roots.cacheDir || "").trim();
    if (cacheDir === String(settings.configuredCacheDir ?? "").trim()) return;
    try {
      await saveSettings({ cacheDir });
      await loadCache();
    } catch (err) {
      toast(err.message || tr("common.error"), "error");
      await loadSettings().catch(() => {});
    }
  }

  async function applyScanDirs() {
    const dirs = String(roots.scanDirs || "").split(/\r?\n/).map((line) => line.trim()).filter(Boolean);
    try {
      await saveSettings({ localScanDirs: dirs });
      await loadCache();
    } catch (err) {
      toast(err.message || tr("common.error"), "error");
      await loadSettings().catch(() => {});
    }
  }

  // A browser cannot return an absolute local path, so the picker lists folders
  // through the server and applies the picked path to one field.
  function openPicker(start, apply) {
    picker.open = true;
    picker.path = String(start || "").trim();
    picker.apply = apply;
  }

  function appendScanDir(picked) {
    const lines = String(roots.scanDirs || "").split(/\r?\n/).map((line) => line.trim()).filter(Boolean);
    if (!lines.includes(picked)) lines.push(picked);
    roots.scanDirs = lines.join("\n");
    applyScanDirs();
  }

  const repos = $derived.by(() => {
    let items = [...(ui.cache.repos || [])];
    if (ui.cache.filter !== "all") items = items.filter((repo) => repo.type === ui.cache.filter);
    const query = ui.cache.search.trim().toLowerCase();
    if (query) items = items.filter((repo) => (repo.repo + " " + repo.owner + " " + repo.name).toLowerCase().includes(query));
    items.sort((a, b) => {
      if (ui.cache.sort === "size") return (b.size || 0) - (a.size || 0);
      if (ui.cache.sort === "date") return String(b.downloaded || "").localeCompare(String(a.downloaded || ""));
      return String(a.repo).localeCompare(String(b.repo));
    });
    return items;
  });

  const locations = $derived(ui.cache.selection?.locations || []);
  const location = $derived(locations.find((item) => item.id === ui.cache.selectedLocationId));
  const stats = $derived(ui.cache.stats);

  async function openDetails(repo, type) {
    try {
      const data = await loadCacheDetails(repo, type);
      ui.cache.selectedLocationId = "";
      ui.cache.selectedGroupId = "";
      ui.cache.outcome = null;
      ui.cache.selection = null;
      if (data.type === "model") {
        const selection = await loadCacheSelection(repo, "").catch(() => null);
        if (selection) ui.cache.selectedLocationId = (selection.locations || [])[0]?.id || "";
      }
    } catch (err) {
      toast(err.message || tr("common.error"), "error");
    }
  }

  async function chooseLocation(event) {
    const id = event.target.value;
    ui.cache.selectedLocationId = id;
    ui.cache.selectedGroupId = "";
    try {
      await loadCacheSelection(ui.cache.details.repo, id);
    } catch (err) {
      toast(err.message, "error");
    }
  }

  function askDeleteRepo(repo, type) {
    confirmRepo = { repo, type };
  }

  // The detail modal is a snapshot of a repository that no longer exists once
  // its whole cache entry is deleted, so the modal has to close with it.
  function closeDetails() {
    ui.cache.details = null;
    ui.cache.selection = null;
    ui.cache.outcome = null;
  }

  async function removeRepo(repo, type) {
    try {
      const result = await deleteCacheRepo(repo, type);
      confirmRepo = null;
      const ok = result.success !== false;
      toast(result.message || tr("common.delete") + ": " + repo, ok ? "success" : "error");
      closeDetails();
      await loadCache();
    } catch (err) {
      toast(err.message || tr("common.error"), "error");
    }
  }

  async function confirmDelete() {
    if (!confirm) return;
    const key = JSON.stringify([confirm.repo, confirm.locationId, confirm.groupId]);
    ui.cache.pendingDeletes.add(key);
    try {
      const outcome = await deleteCacheSelection({ repo: confirm.repo, type: "model", locationId: confirm.locationId, groupId: confirm.groupId, members: confirm.members });
      ui.cache.outcome = outcome;
      toast(outcome.message || tr("common.deleted"), outcome.kind === "success" ? "success" : "warning");
    } catch (err) {
      toast(err.message || tr("common.error"), "error");
    } finally {
      ui.cache.pendingDeletes.delete(key);
      confirm = null;
    }
  }

  function statusBadge(repo) {
    if (repo.downloadStatus === "complete") return "cache-badge-complete";
    if (repo.downloadStatus === "filtered") return "cache-badge-filtered";
    if (repo.manifest) return "cache-badge-manifest";
    return "";
  }
</script>

<header class="page-header">
  <div>
    <h1>{tr("nav.cache")}</h1>
    <p class="page-subtitle">{tr("cache.subtitle")}</p>
  </div>
  <div class="page-actions">
    <button class="btn btn-outline" type="button" id="cacheCheckBtn" title={tr("cache.checkHint")} disabled={ui.cache.busy || checkRunning} onclick={() => startChecked({})}>
      {#if checkRunning}<span class="spinner-sm"></span>{/if}<Icon name="check" size={18} /> {checkRunning ? tr("cache.check.running") : tr("cache.check")}
    </button>
    <button class="btn btn-outline" type="button" title={tr("cache.rebuildHint")} disabled={ui.cache.busy} onclick={runRebuild}>
      {#if rebuilding}<span class="spinner-sm"></span>{/if}<Icon name="rebuild" size={18} /> {tr("cache.rebuild")}
    </button>
    <button class="btn btn-secondary" type="button" disabled={ui.cache.busy} onclick={() => loadCache().catch((err) => toast(err.message, "error"))}>
      <Icon name="refresh" size={18} /> {tr("common.refresh")}
    </button>
  </div>
</header>

{#if stats}
  <div id="cacheStats" class="cache-stats">
    <div class="stat-card"><div class="stat-icon stat-icon-models"><Icon name="cubePlain" size={24} /></div><div class="stat-content"><div class="stat-value">{stats.totalModels || 0}</div><div class="stat-label">{tr("cache.statModels")}</div></div></div>
    <div class="stat-card"><div class="stat-icon stat-icon-datasets"><Icon name="book" size={24} /></div><div class="stat-content"><div class="stat-value">{stats.totalDatasets || 0}</div><div class="stat-label">{tr("cache.statDatasets")}</div></div></div>
    <div class="stat-card"><div class="stat-icon stat-icon-size"><Icon name="cube" size={24} /></div><div class="stat-content"><div class="stat-value">{stats.totalSizeHuman || "0 B"}</div><div class="stat-label">{tr("cache.statSize")}</div></div></div>
    <div class="stat-card"><div class="stat-icon stat-icon-files"><Icon name="file" size={24} /></div><div class="stat-content"><div class="stat-value">{stats.totalFiles || 0}</div><div class="stat-label">{tr("cache.statFiles")}</div></div></div>
  </div>
{/if}

<div id="cacheLocations" class="cache-locations">
  <div class="cache-location-head">
    <Icon name="folder" size={16} />
    <span>{tr("cache.locations")}</span>
    <span class="cache-location-hint">{tr("cache.locationsHint")}</span>
  </div>
  <div class="cache-location-rows">
    <div class="cache-location-row" id="cacheHubDirGroup">
      <label class="cache-location-label" for="cacheHubDir" title={tr("settings.hubDirHint")}>{tr("settings.hubDir")}</label>
      <div class="input-with-browse">
        <input type="text" id="cacheHubDir" class="path-input" readonly spellcheck="false" aria-readonly="true" placeholder={tr("common.unavailable")} value={settings.effectiveHubDir || ""} title={`${tr("settings.hubDirHint")}${settings.effectiveHubDir ? ": " + settings.effectiveHubDir : ""}`} />
        <button type="button" class="btn btn-secondary btn-sm" title={tr("settings.hubDirBrowseHint")} onclick={() => openPicker(settings.effectiveHubDir || roots.cacheDir, (picked) => { roots.cacheDir = picked; applyCacheDir(); })}>{tr("settings.browse")}</button>
      </div>
    </div>
    <div class="cache-location-row" id="cacheDirGroup">
      <label class="cache-location-label" for="cacheDirInput" title={tr("settings.cacheDirHint")}>{tr("settings.cacheDir")}</label>
      <div class="input-with-browse">
        <input type="text" id="cacheDirInput" class="path-input" placeholder={tr("settings.defaultHubCache")} spellcheck="false" bind:value={roots.cacheDir} onchange={applyCacheDir} />
        <button type="button" class="btn btn-secondary btn-sm" title={tr("settings.cacheDirHint")} onclick={() => openPicker(roots.cacheDir, (picked) => { roots.cacheDir = picked; applyCacheDir(); })}>{tr("settings.browse")}</button>
      </div>
    </div>
    <div class="cache-location-row cache-location-row-multi" id="localScanDirsGroup">
      <label class="cache-location-label" for="localScanDirs" title={tr("settings.localScanDirsHint")}>{tr("settings.localScanDirs")}</label>
      <div class="input-with-browse">
        <textarea id="localScanDirs" rows="2" class="path-input" spellcheck="false" bind:value={roots.scanDirs} onchange={applyScanDirs}></textarea>
        <button type="button" class="btn btn-secondary btn-sm" title={tr("settings.localScanDirsHint")} onclick={() => openPicker(String(roots.scanDirs || "").split(/\r?\n/)[0], appendScanDir)}>{tr("settings.browse")}</button>
      </div>
    </div>
  </div>
</div>

<div class="cache-toolbar">
  <div class="cache-search">
    <span class="search-icon"><Icon name="search" size={18} /></span>
    <input type="text" id="cacheSearch" placeholder={tr("cache.searchPlaceholder")} autocomplete="off" bind:value={ui.cache.search} />
  </div>
  <div class="cache-filters">
    <button class="filter-btn{ui.cache.filter === "all" ? " active" : ""}" type="button" data-filter="all">{tr("cache.filter.all")}</button>
    <button class="filter-btn{ui.cache.filter === "model" ? " active" : ""}" type="button" data-filter="model"><Icon name="cubePlain" size={14} /> {tr("cache.filter.model")}</button>
    <button class="filter-btn{ui.cache.filter === "dataset" ? " active" : ""}" type="button" data-filter="dataset"><Icon name="book" size={14} /> {tr("cache.filter.dataset")}</button>
  </div>
  <div class="cache-sort">
    <label for="cacheSort">{tr("cache.sort.label")}</label>
    <select id="cacheSort" bind:value={ui.cache.sort}>
      <option value="name">{tr("cache.sort.name")}</option>
      <option value="size">{tr("cache.sort.size")}</option>
      <option value="date">{tr("cache.sort.date")}</option>
    </select>
  </div>
  <div class="cache-view-toggle">
    <button class="view-btn{ui.cache.view === "grid" ? " active" : ""}" type="button" data-view="grid" title={tr("cache.gridView")}><Icon name="grid" size={18} /></button>
    <button class="view-btn{ui.cache.view === "list" ? " active" : ""}" type="button" data-view="list" title={tr("cache.listView")}><Icon name="list" size={18} /></button>
  </div>
</div>

{#if ui.cache.warnings.length}
  <div class="cache-warnings" role="status" id="cacheWarnings">
    {#each ui.cache.warnings as warning (warning)}
      <p class="form-hint">{warning}</p>
    {/each}
  </div>
{/if}

{#if check}
  <section class="cache-check" id="cacheCheck" role="status" aria-live="polite">
    <div class="cache-check-head">
      <span class="cache-check-state">
        {check.phase === "cancelling" ? tr("cache.check.cancelling") : tr("cache.check." + check.state)}
      </span>
      <span class="cache-check-count">{tf("cache.check.progress", ui.lang, { done: check.doneRepos, total: check.totalRepos })}</span>
      {#if checkRunning}
        <button class="btn btn-ghost btn-sm" id="cacheCheckCancel" type="button" onclick={() => cancelCacheCheck(check.id).catch((err) => toast(err.message || tr("common.error"), "error"))}>{tr("cache.check.cancel")}</button>
      {/if}
    </div>
    <div class="progress-bar"><div class="progress-fill" style="width: max(3px, {checkPercent}%)"></div></div>
    {#if check.currentRepo}
      <p class="form-hint">{tf("cache.check.repo", ui.lang, { repo: check.currentRepo })}</p>
    {/if}
    {#if check.error}
      <p class="form-hint">{check.error}</p>
    {/if}
    {#if checkResult}
      <p class="cache-check-mode">{checkResult.verifyBytes ? tr("cache.check.modeBytes") : tr("cache.check.modeQuick")}</p>
      <div class="rebuild-stats">
        <div class="rebuild-stat"><span class="rebuild-stat-value">{checkResult.reposChecked || 0}</span><span class="rebuild-stat-label">{tr("cache.check.statChecked")}</span></div>
        <div class="rebuild-stat"><span class="rebuild-stat-value">{checkResult.reposClean || 0}</span><span class="rebuild-stat-label">{tr("cache.check.statClean")}</span></div>
        <div class="rebuild-stat"><span class="rebuild-stat-value">{checkResult.reposProblem || 0}</span><span class="rebuild-stat-label">{tr("cache.check.statProblems")}</span></div>
        <div class="rebuild-stat"><span class="rebuild-stat-value">{checkResult.reposUnreadable || 0}</span><span class="rebuild-stat-label">{tr("cache.check.statUnreadable")}</span></div>
        <div class="rebuild-stat"><span class="rebuild-stat-value">{checkResult.blobs || 0}</span><span class="rebuild-stat-label">{tr("cache.check.statBlobs")}</span></div>
        <div class="rebuild-stat"><span class="rebuild-stat-value">{checkResult.bytesHuman || "0 B"}</span><span class="rebuild-stat-label">{tr("cache.check.statBytes")}</span></div>
      </div>
      {#if checkResult.stagingFiles}
        <p class="form-hint">{tf("cache.check.staging", ui.lang, { n: checkResult.stagingFiles, size: formatBytes(checkResult.stagingBytes || 0) })}</p>
      {/if}
      {#if problemRepos.length}
        <ul class="cache-check-problems" id="cacheCheckProblems">
          {#each problemRepos as item (item.repo)}
            <li class="cache-check-repo">
              <div class="cache-check-repo-head">
                <span class="cache-check-repo-name">{item.repo}</span>
                <span class="cache-check-repo-meta">{tf("cache.check.blobCount", ui.lang, { n: item.blobs || 0, size: formatBytes(item.bytes || 0) })}</span>
              </div>
              {#if item.readError}
                <p class="cache-check-problem">{checkKind("unreadable")} · {item.readError}</p>
              {/if}
              {#each item.problems || [] as problem (`${item.repo}/${problem.kind}/${problem.path}`)}
                <p class="cache-check-problem">
                  <span>{checkKind(problem.kind)}</span>
                  {#if problem.path}<code>{problem.path}</code>{/if}
                  {#if problem.detail}<span class="cache-check-detail">{problem.detail}</span>{/if}
                </p>
              {/each}
              {#if item.problemsOmitted}
                <p class="form-hint" id="cacheCheckMoreFindings">{tf("cache.check.problemsOmitted", ui.lang, { n: item.problemsOmitted })}</p>
              {/if}
            </li>
          {/each}
        </ul>
        {#if checkResult.reposOmitted}
          <p class="form-hint" id="cacheCheckMoreRepos">{tf("cache.check.reposOmitted", ui.lang, { n: checkResult.reposOmitted })}</p>
        {/if}
      {:else if !checkRunning && (checkResult.reposChecked || 0) > 0}
        <p class="cache-check-clean" id="cacheCheckClean">{tr("cache.check.allIntact")}</p>
      {/if}
      {#if !checkRunning && !checkResult.verifyBytes && (checkResult.reposChecked || 0) > 0}
        <button class="btn btn-outline btn-sm" id="cacheCheckBytes" type="button" title={tr("cache.check.bytesHint")} onclick={() => startChecked({ verifyBytes: true })}>{tr("cache.check.runBytes")}</button>
      {/if}
    {/if}
  </section>
{/if}

<div id="cacheList" class="cache-list cache-{ui.cache.view}-view">
  {#if ui.cache.busy}
    <div class="loading-state"><div class="spinner"><Icon name="spinner" size={18} cls="spin" /></div><p>{tr("common.loading")}</p></div>
  {:else if repos.length === 0 && ui.cache.loadFailed}
    <div class="empty-state">
      <div class="empty-icon"><Icon name="folder" size={64} /></div>
      <h3>{tr("common.error")}</h3>
      <p>{tr("cache.loadFailed")}</p>
      {#if ui.cache.hubDir}<p class="cache-dir-hint">{ui.cache.hubDirIsHub ? tr("cache.hubCache") : tr("cache.cacheLabel")}: {ui.cache.hubDir}</p>{/if}
    </div>
  {:else if repos.length === 0}
    <div class="empty-state">
      <div class="empty-icon"><Icon name="folder" size={64} /></div>
      <h3>{ui.cache.repos.length === 0 ? tr("cache.emptyTitle") : tr("cache.noResults")}</h3>
      <p>{ui.cache.repos.length === 0 ? tr("cache.emptyHint") : tr("cache.noResultsHint")}</p>
      {#if ui.cache.hubDir}<p class="cache-dir-hint">{ui.cache.hubDirIsHub ? tr("cache.hubCache") : tr("cache.cacheLabel")}: {ui.cache.hubDir}</p>{/if}
    </div>
  {:else if ui.cache.view === "grid"}
    {#each repos as repo (repo.repo)}
      {@const caps = mergeCapabilities(repo.capabilities, detectSearchCapabilities({ id: repo.repo, tags: repo.tags }).capabilities)}
      {@const titles = detectSearchCapabilities({ id: repo.repo, tags: repo.tags }).titles}
      <div class="cache-card" role="button" tabindex="0" onclick={() => openDetails(repo.repo, repo.type)}>
        <div class="cache-card-header">
          <span class="cache-card-type cache-type-{repo.type}"><Icon name={repo.type === "model" ? "cubePlain" : "book"} size={16} /> {typeLabel(repo.type)}</span>
          <span class="cache-card-badges">
            {#if repo.source}<span class="cache-badge cache-source-badge">{repo.source}</span>{/if}
            {#if statusBadge(repo)}<span class="cache-badge {statusBadge(repo)}" title={repo.manifest?.filters ? tf("cache.filteredDownloadTitle", ui.lang, { filters: repo.manifest.filters }) : ""}>{statusLabel(repo.downloadStatus)}</span>{/if}
          </span>
        </div>
        <div class="cache-card-body">
          <div class="cache-card-owner">{repo.owner}</div>
          <div class="cache-card-name">{repo.name}</div>
          <CapabilityIcons capabilities={caps} titles={{ ...titles, vision: repo.hasMMProj ? "capability.visionWithMmproj" : titles.vision }} />
          {#if (repo.quantizations || []).length}
            <div class="cache-quant-subtitle" title={repo.quantizations.join(", ")}>{repo.quantizations.slice(0, 8).join(", ")}{repo.quantizations.length > 8 ? " +" + (repo.quantizations.length - 8) : ""}</div>
          {/if}
        </div>
        <div class="cache-card-meta">
          <div class="cache-card-stat"><Icon name="cube" size={14} /> {repo.sizeHuman}</div>
          <div class="cache-card-stat"><Icon name="file" size={14} /> {tf("cache.cardFiles", ui.lang, { n: repo.fileCount })}</div>
        </div>
        <div class="cache-card-footer">
          {#if repo.commit}<code class="cache-commit" title={tr("cache.detailCommit") + ": " + repo.commit}>{repo.commit}</code>{/if}
          {#if repo.downloaded}<span class="cache-date">{repo.downloaded}</span>{/if}
        </div>
      </div>
    {/each}
  {:else}
    <div class="cache-table">
      <div class="cache-table-header">
        <div class="cache-col-type">{tr("common.colType")}</div>
        <div class="cache-col-repo">{tr("common.colRepo")}</div>
        <div class="cache-col-source">{tr("common.colSource")}</div>
        <div class="cache-col-size">{tr("common.colSize")}</div>
        <div class="cache-col-files">{tr("common.colFiles")}</div>
        <div class="cache-col-date">{tr("common.colDownloaded")}</div>
        <div class="cache-col-actions"></div>
      </div>
      {#each repos as repo (repo.repo)}
        {@const caps = mergeCapabilities(repo.capabilities, detectSearchCapabilities({ id: repo.repo, tags: repo.tags }).capabilities)}
        {@const titles = detectSearchCapabilities({ id: repo.repo, tags: repo.tags }).titles}
        <div class="cache-table-row" role="button" tabindex="0" onclick={() => openDetails(repo.repo, repo.type)}>
          <div class="cache-col-type">
            <span class="cache-type-badge cache-type-{repo.type}"><Icon name={repo.type === "model" ? "cubePlain" : "book"} size={14} /> {typeLabel(repo.type)}</span>
          </div>
          <div class="cache-col-repo">
            <span class="cache-repo-name">{repo.repo}</span>
            <CapabilityIcons capabilities={caps} titles={{ ...titles, vision: repo.hasMMProj ? "capability.visionWithMmproj" : titles.vision }} />
            {#if (repo.quantizations || []).length}
              <div class="cache-quant-subtitle" title={repo.quantizations.join(", ")}>{repo.quantizations.slice(0, 8).join(", ")}{repo.quantizations.length > 8 ? " +" + (repo.quantizations.length - 8) : ""}</div>
            {/if}
            {#if statusBadge(repo)}<span class="cache-badge {statusBadge(repo)}">{statusLabel(repo.downloadStatus)}</span>{/if}
          </div>
          <div class="cache-col-source">{#if repo.source}<span class="cache-badge cache-source-badge">{repo.source}</span>{/if}</div>
          <div class="cache-col-size">{repo.sizeHuman}</div>
          <div class="cache-col-files">{repo.fileCount}</div>
          <div class="cache-col-date">{repo.downloaded || "-"}</div>
          <div class="cache-col-actions">
            <button class="btn btn-ghost btn-sm" type="button" title={tr("common.viewDetails")} onclick={(event) => { event.stopPropagation(); openDetails(repo.repo, repo.type); }}>
              <Icon name="info" size={16} />
            </button>
          </div>
        </div>
      {/each}
    </div>
  {/if}
</div>

<Modal title={tr("cache.detailTitle")} open={Boolean(ui.cache.details)} onclose={closeDetails}>
  {#if ui.cache.details}
    <p class="cache-selection-readonly">{tr("cache.detailSnapshotNote")}</p>
    <div class="cache-detail-modal">
      <div class="cache-detail-header">
        <span class="cache-detail-type cache-type-{ui.cache.details.type}"><Icon name={ui.cache.details.type === "model" ? "cubePlain" : "book"} size={20} /> {typeLabel(ui.cache.details.type)}</span>
        {#if ui.cache.details.downloadStatus === "complete"}<span class="cache-badge cache-badge-complete">{tr("cache.badgeComplete")}</span>{/if}
        {#if ui.cache.details.downloadStatus === "filtered"}<span class="cache-badge cache-badge-filtered">{tr("cache.badgeFiltered")}</span>{/if}
        {#if ui.cache.details.downloadStatus === "unknown"}<span class="cache-badge cache-badge-unknown">{tr("cache.badgeExternal")}</span>{/if}
        {#if ui.cache.details.source}<span class="cache-badge cache-source-badge">{ui.cache.details.source}</span>{/if}
        <CapabilityIcons capabilities={ui.cache.details.capabilities || []} titles={{ vision: ui.cache.details.hasMMProj ? "capability.visionWithMmproj" : "capability.title.vision" }} />
        <div class="cache-detail-repo">
          <span class="cache-detail-owner">{ui.cache.details.owner}/</span>
          <span class="cache-detail-name">{ui.cache.details.name}</span>
        </div>
      </div>

      <div class="cache-detail-stats">
        <div class="cache-detail-stat"><div class="cache-detail-stat-value">{ui.cache.details.sizeHuman}</div><div class="cache-detail-stat-label">{tr("cache.statSize")}</div></div>
        <div class="cache-detail-stat"><div class="cache-detail-stat-value">{ui.cache.details.fileCount}</div><div class="cache-detail-stat-label">{tr("cache.statFiles")}</div></div>
        <div class="cache-detail-stat"><div class="cache-detail-stat-value">{ui.cache.details.branch || "main"}</div><div class="cache-detail-stat-label">{tr("cache.detailBranch")}</div></div>
        {#if ui.cache.details.commit}<div class="cache-detail-stat"><div class="cache-detail-stat-value"><code>{ui.cache.details.commit}</code></div><div class="cache-detail-stat-label">{tr("cache.detailCommit")}</div></div>{/if}
      </div>

      {#if ui.cache.details.manifest}
        <div class="cache-detail-section">
          <h4>{tr("cache.detailDownloadInfo")}</h4>
          <div class="cache-detail-grid">
            <div class="cache-detail-item">
              <span class="cache-detail-label">{tr("cache.detailStatus")}</span>
              <span class="cache-detail-value">{ui.cache.details.downloadStatus === "complete" ? "✓ " + tr("cache.completeDownload") : ui.cache.details.downloadStatus === "filtered" ? "◐ " + tr("cache.filteredDownload") : ""}{ui.cache.details.manifest.filters ? " (" + ui.cache.details.manifest.filters + ")" : ""}</span>
            </div>
            <div class="cache-detail-item"><span class="cache-detail-label">{tr("cache.detailDownloaded")}</span><span class="cache-detail-value">{ui.cache.details.manifest.downloaded}</span></div>
            {#if ui.cache.details.manifest.command}<div class="cache-detail-item cache-detail-full"><span class="cache-detail-label">{tr("cache.detailCommand")}</span><code class="cache-detail-code">{ui.cache.details.manifest.command}</code></div>{/if}
          </div>
        </div>
      {:else if ui.cache.details.downloadStatus === "unknown"}
        <div class="cache-detail-section">
          <h4>{tr("cache.detailDownloadInfo")}</h4>
          <div class="cache-detail-note"><Icon name="info" size={16} /><span>{tr("cache.detailExternalNote")}</span></div>
        </div>
      {/if}

      {#if (ui.cache.details.mmprojFiles || []).length}
        <div class="cache-detail-section">
          <h4>{tr("cache.detailVisionEncoder")}</h4>
          <div class="cache-files-list">
            {#each ui.cache.details.mmprojFiles as file (file)}
              <div class="cache-file-row"><span class="cache-file-name" title={file}>{file}</span></div>
            {/each}
          </div>
        </div>
      {/if}

      <div class="cache-detail-section">
        <h4>{tr("cache.detailPaths")}</h4>
        <div class="cache-detail-paths">
          <div class="cache-path-item"><span class="cache-path-label">{ui.cache.details.source === "HF cache" || !ui.cache.details.source ? tr("cache.pathCache") : tr("cache.pathLocal")}</span><code class="cache-path-value">{ui.cache.details.path}</code></div>
          {#if ui.cache.details.friendlyPath}<div class="cache-path-item"><span class="cache-path-label">{tr("cache.pathFriendly")}</span><code class="cache-path-value">{ui.cache.details.friendlyPath}</code></div>{/if}
        </div>
      </div>

      {#if (ui.cache.details.files || []).length}
        <div class="cache-detail-section">
          <h4>{tr("cache.statFiles")} ({ui.cache.details.files.length})</h4>
          <div class="cache-files-list">
            {#each ui.cache.details.files.slice(0, 20) as file (file.name)}
              <div class="cache-file-row">
                <span class="cache-file-name" title={file.name}>{#if file.isLfs}<span class="lfs-badge">LFS</span>{/if}{file.name}</span>
                <span class="cache-file-size">{file.sizeHuman}</span>
              </div>
            {/each}
            {#if ui.cache.details.files.length > 20}
              <div class="cache-files-more">{tf("cache.moreFiles", ui.lang, { n: ui.cache.details.files.length - 20 })}</div>
            {/if}
          </div>
        </div>
      {/if}

      <div class="cache-detail-actions">
        <button class="btn btn-danger" type="button" onclick={() => askDeleteRepo(ui.cache.details.repo, ui.cache.details.type)}>
          <Icon name="trash" size={16} /> {tr("cache.deleteEntire")}
        </button>
      </div>
    </div>

    {#if ui.cache.details.type === "model"}
      <section id="cacheSelectionSection" class="cache-detail-section">
        <h4>{tr("cache.ggufByLocation")}</h4>
        <p>{tr("cache.chooseLocationHint")}</p>
        {#if locations.length}
          <label for="cacheSelectionLocation">{tr("cache.storageLocation")}</label>
          <select id="cacheSelectionLocation" onchange={chooseLocation}>
            {#if ui.cache.selectedLocationId && !location}<option value="-1" selected>{tr("cache.chooseLocationOption")}</option>{/if}
            {#each locations as item, index (item.id)}
              <option value={item.id} selected={item.id === ui.cache.selectedLocationId}>{item.source} — {item.path}</option>
            {/each}
          </select>
        {:else}
          <p>{tr("cache.noSelectableLocations")}</p>
        {/if}

        {#if location}
          <p><strong>{tf("cache.locationPrefix", ui.lang, { source: location.source })}:</strong> <code>{location.path}</code></p>
          {#if location.warning}<p class="cache-selection-warning">{location.warning}</p>{/if}
          {#if (location.groups || []).length}
            {#each location.groups as group (group.id)}
              {@const canDelete = location.canDelete === true && group.canDelete === true}
              {@const disabledReason = location.canDelete !== true ? location.deleteReason || location.warning || tr("cache.locationNotEligible") : group.canDelete !== true ? group.warning || tr("cache.groupNotEligible") : ""}
              {@const pending = ui.cache.pendingDeletes.has(JSON.stringify([ui.cache.details.repo, location.id, group.id]))}
              <article class="cache-selection-group">
                <div class="cache-selection-group-heading">
                  <div><strong>{group.label}</strong>{#if group.quant} <span class="cache-badge">{group.quant}</span>{/if}</div>
                  <span>{tf("cache.cardFiles", ui.lang, { n: new Set((group.members || []).map((member) => member.path)).size })}{new Set((group.members || []).flatMap((member) => member.versions || [])).size ? " · " + tf("cache.savedVersions", ui.lang, { n: new Set((group.members || []).flatMap((member) => member.versions || [])).size }) : ""}</span>
                </div>
                <details><summary>{tf("cache.showFileRecords", ui.lang, { n: (group.members || []).length })}</summary>
                  <ul>
                    {#each group.members || [] as member (member.path)}
                      <li><code>{member.path}</code>{#if (member.versions || []).length} <span>({member.versions.join(", ")})</span>{/if}{#if member.linkOnly} <span class="cache-selection-warning">{tr("cache.linkOnly")} — {member.message || tr("cache.linkOnlyRemains")}</span>{/if}</li>
                    {/each}
                  </ul>
                </details>
                {#if group.warning}<p class="cache-selection-warning">{group.warning}</p>{/if}
                <button class="btn btn-secondary cache-selection-pick" type="button" aria-pressed={group.id === ui.cache.selectedGroupId} onclick={() => (ui.cache.selectedGroupId = group.id)}>{group.id === ui.cache.selectedGroupId ? tr("cache.selectedGroup") : tr("cache.selectThisGroup")}</button>
                <button class="btn btn-danger cache-selection-delete" type="button" disabled={!canDelete || pending} aria-describedby={"cache-selection-help-" + group.id} title={disabledReason} onclick={() => (confirm = { repo: ui.cache.details.repo, locationId: location.id, groupId: group.id, label: group.label, source: location.source, path: location.path, members: group.members })}>{tr("cache.deleteGroup")}</button>
                <span id={"cache-selection-help-" + group.id} class="cache-selection-readonly">{pending ? tr("cache.deletionInProgress") : disabledReason ? disabledReason : tr("cache.onlyThisGroup")}</span>
              </article>
            {/each}
          {:else}
            <p>{tr("cache.noGroupsInLocation")}</p>
          {/if}
        {/if}

        {#if ui.cache.selectedLocationId && !location}
          <p class="cache-selection-warning">{tr("cache.staleLocationWarning")}</p>
        {/if}

        {#if ui.cache.outcome}
          <div class="cache-selection-outcome {ui.cache.outcome.kind === "success" ? "" : "cache-selection-warning"}" role="status">
            <h4>{ui.cache.outcome.kind === "success" && (ui.cache.outcome.retainedPayloads || []).length ? tr("cache.outcomeRemovedShared") : ui.cache.outcome.kind === "success" ? tr("cache.outcomeRemoved") : ui.cache.outcome.kind === "partial" ? tr("cache.outcomePartial") : tr("cache.outcomeNotConfirmed")}</h4>
            <p>{tf("cache.outcomeDetail", ui.lang, { repo: ui.cache.details.repo, source: location?.source, path: location?.path, group: ui.cache.outcome.group || ui.cache.selectedGroupId })}</p>
            {#if ui.cache.outcome.message}<p>{ui.cache.outcome.message}</p>{/if}
            {#if (ui.cache.outcome.removed || []).length}<p>{tr("cache.outcomeRemovedLabel")}</p><ul>{#each ui.cache.outcome.removed as path (path)}<li><code>{path}</code></li>{/each}</ul>{/if}
            {#if (ui.cache.outcome.remaining || []).length}<p>{tr("cache.outcomeRemainingLabel")}</p><ul>{#each ui.cache.outcome.remaining as path (path)}<li><code>{path}</code></li>{/each}</ul>{/if}
            {#if (ui.cache.outcome.errors || []).length}<p>{tr("cache.outcomeErrorsLabel")}</p><ul>{#each ui.cache.outcome.errors as error (error)}<li>{error}</li>{/each}</ul>{/if}
            {#if (ui.cache.outcome.linkOnlyEntries || []).length}<p>{tr("cache.outcomeLinksRemoved")}</p><ul>{#each ui.cache.outcome.linkOnlyEntries as path (path)}<li><code>{path}</code></li>{/each}</ul>{/if}
            {#if (ui.cache.outcome.retainedPayloads || []).length}<p>{tr("cache.outcomeRetained")}</p><ul>{#each ui.cache.outcome.retainedPayloads as path (path)}<li><code>{path}</code></li>{/each}</ul>{/if}
          </div>
        {/if}

        {#if ui.cache.selectedGroupId}
          <p class="cache-selection-readonly">{tr("cache.groupSelectedHere")}</p>
        {/if}
        <p id="cache-selection-readonly" class="cache-selection-readonly">{tr("cache.onlySelectedGroup")}</p>
      </section>
    {/if}
  {/if}
</Modal>

<Modal title={tr("cache.confirmTitle")} open={Boolean(confirm)} onclose={() => (confirm = null)}>
  {#if confirm}
    <div class="delete-confirm cache-selection-confirmation">
      <p>{tr("cache.confirmRepository")}: <strong>{confirm.repo}</strong></p>
      <p>{tr("cache.confirmType")}: <strong>{tr("cache.confirmModel")}</strong></p>
      <p>{tr("cache.storageLocation")}: <strong>{confirm.source}</strong><br /><code>{confirm.path}</code></p>
      <p>{tr("cache.confirmGroup")}: <strong>{confirm.label}</strong></p>
      <p>{confirm.members.some((member) => (member.versions || []).length) ? tr("cache.confirmNoteVersions") : tr("cache.confirmNoteMembers")}</p>
      {#if confirm.members.some((member) => member.linkOnly)}<p class="cache-selection-warning">{tr("cache.confirmLinkOnlyWarning")}</p>{/if}
      <details open><summary>{tf("cache.confirmFilesIncluded", ui.lang, { n: new Set(confirm.members.map((member) => member.path)).size })}</summary>
        <ul>
          {#each confirm.members as member (member.path)}
            <li><code>{member.path}</code>{#if (member.versions || []).length}<span> — {tr("cache.savedVersionsLabel")} ({member.versions.length}): {member.versions.join(", ")}</span>{/if}{#if member.linkOnly}<strong> — {tr("cache.confirmLinkOnlyMember")}</strong>{/if}</li>
          {/each}
        </ul>
      </details>
      <p id="cache-delete-progress" role="status"></p>
      <div class="form-actions">
        <button class="btn btn-ghost" type="button" onclick={() => (confirm = null)}>{tr("common.cancel")}</button>
        <button class="btn btn-danger" type="button" onclick={confirmDelete}>{tr("cache.confirmDeleteButton")}</button>
      </div>
    </div>
  {/if}
</Modal>
<Modal title={tr("cache.deleteTitle")} open={Boolean(confirmRepo)} onclose={() => (confirmRepo = null)}>
  {#if confirmRepo}
    <div class="delete-confirm">
      <div class="delete-warning"><Icon name="warning" size={48} /></div>
      <p class="delete-message">{tf("cache.deleteQuestion", ui.lang, { repo: confirmRepo.repo })}</p>
      <p class="delete-note">{tf("cache.deleteNote", ui.lang, { type: confirmRepo.type })}</p>
      <div class="form-actions">
        <button class="btn btn-ghost" type="button" onclick={() => (confirmRepo = null)}>{tr("common.cancel")}</button>
        <button class="btn btn-danger" type="button" onclick={() => removeRepo(confirmRepo.repo, confirmRepo.type)}>
          <Icon name="trash" size={16} /> {tr("common.delete")}
        </button>
      </div>
    </div>
  {/if}
</Modal>

<Modal title={tr("settings.folderPicker")} open={picker.open} onclose={() => (picker.open = false)}>
  {#if picker.open}
    <FolderBrowser path={picker.path} onpick={(picked) => { picker.apply?.(picked); picker.open = false; }} />
  {/if}
</Modal>

<Modal title={tr("cache.rebuildComplete")} open={Boolean(rebuildResult)} onclose={() => (rebuildResult = null)}>
  {#if rebuildResult}
    <div class="rebuild-result">
      <div class="rebuild-success"><Icon name="check" size={48} /></div>
      <p class="rebuild-message">{rebuildResult.message || tr("cache.rebuildComplete")}</p>
      <div class="rebuild-stats">
        <div class="rebuild-stat"><span class="rebuild-stat-value">{rebuildResult.reposScanned || 0}</span><span class="rebuild-stat-label">{tr("cache.rebuildStatRepos")}</span></div>
        <div class="rebuild-stat"><span class="rebuild-stat-value">{rebuildResult.symlinksCreated || 0}</span><span class="rebuild-stat-label">{tr("cache.rebuildStatCreated")}</span></div>
        <div class="rebuild-stat"><span class="rebuild-stat-value">{rebuildResult.symlinksUpdated || 0}</span><span class="rebuild-stat-label">{tr("cache.rebuildStatUpdated")}</span></div>
        <div class="rebuild-stat"><span class="rebuild-stat-value">{rebuildResult.orphansRemoved || 0}</span><span class="rebuild-stat-label">{tr("cache.rebuildStatOrphans")}</span></div>
      </div>
      {#if (rebuildResult.errors || []).length}
        <div class="rebuild-errors">{(rebuildResult.errors || []).join("; ")}</div>
      {/if}
    </div>
  {/if}
</Modal>
