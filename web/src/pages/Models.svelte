<script>
  import { onDestroy } from "svelte";
  import { ui, loadSearch, loadAnalyze, loadReadme, clearModelsDetail, loadDiskFree, loadHardware, saveSettings, useHubSource, previewPlan, startDownload, toast, dlStatusFor, setDlStatus, checkDownloadDiskSpace } from "../store.svelte.js";
  import { t, tf } from "../i18n.js";
  import { activeHubId, hubSourceLabel, hubSources, DEFAULT_HF_ENDPOINT } from "../source.js";
  import { formatNumber, formatBytes, gradeClass, fitTitle } from "../format.js";
  import { detectSearchCapabilities } from "../capabilities.js";
  import { getConfiguredRouteOptions, routeKeyForAnalysis } from "../routes.js";
  import { renderMarkdown, escapeHtml } from "../markdown.js";
  import Icon from "../components/Icon.svelte";
  import EmptyState from "../components/EmptyState.svelte";
  import CapabilityIcons from "../components/CapabilityIcons.svelte";
  import Modal from "../components/Modal.svelte";
  import GpuCombobox from "../components/GpuCombobox.svelte";

  const tr = (key) => t(key, ui.lang);
  // The hardware bar is where the hardware override is edited: one box per GPU
  // position, plus system RAM and context length. Picking a card fills VRAM,
  // bandwidth and core count from the catalog row, so a wrong detection can be
  // corrected in place.
  let hwSlots = $state([]);
  let hwForm = $state({ gpu_count: "", system_ram_gb: "", context_length: "" });
  let hwSaving = $state(false);
  let hwTimer = $state(0);
  // Only fields the user actually touched become overrides; the rest keep their
  // detected source, so picking a card does not pin every number.
  // Plain object on purpose: the sync effect below must not read it, otherwise
  // marking a field re-runs that effect and rebuilds the slots from the server,
  // which drops the pick the user just made. Nothing renders from these flags.
  const hwDirty = {};

  const detectedGpus = $derived((ui.hardware?.detected_gpus || []).filter((gpu) => gpu.name));
  const hwSources = $derived(ui.hardware?.sources || {});
  const gpuOptions = $derived([
    ...detectedGpus.map((gpu) => ({ name: gpu.name, vram_gb: gpu.vram_gb, bandwidth_gbps: gpu.bandwidth_gbps, cores: gpu.cores })),
    ...Object.entries(ui.hardware?.catalog_entries || {})
      .map(([name, entry]) => ({ name, ...entry }))
      .filter((entry) => !detectedGpus.some((gpu) => gpu.name === entry.name))
  ]);

  $effect(() => {
    const gpus = ui.hardware?.gpus;
    if (!gpus) return;
    // Built from the response alone: reading hwSlots here would make this effect a
    // reader of the state it writes, which Svelte reports as effect_update_depth_exceeded.
    const slots = gpus.map((gpu) => ({
      name: gpu.name || "",
      vram_gb: gpu.vram_gb ?? "",
      bandwidth_gbps: gpu.bandwidth_gbps ?? "",
      cores: gpu.cores ?? ""
    }));
    hwSlots = slots;
    hwForm.gpu_count = ui.hardware?.effective?.gpu_count ?? "";
    hwForm.system_ram_gb = ui.hardware?.effective?.system_ram_gb ?? "";
    hwForm.context_length = ui.hardware?.effective?.context_length ?? "";
    // A smaller GPU count drops positions, so their pending edits must go too.
    for (const key of Object.keys(hwDirty)) {
      if (key.startsWith("slot:") && Number(key.split(":")[1]) >= slots.length) delete hwDirty[key];
    }
  });

  // Core counts are exact spec figures, so they read as a grouped number rather
  // than the abbreviated form used for sizes.
  const coreCount = (n) => (n ? Number(n).toLocaleString("en-US") : "?");

  // A blank field means "use what was detected", so the body keeps the parts of
  // the override block this bar does not show (context length, offload).
  function hardwareBody() {
    const body = { ...(ui.settings?.hardware || {}) };
    // Positions are the boxes the bar shows, so removing one drops its override.
    const gpus = hwSlots.map((slot, index) => {
      const entry = { ...(body.gpus?.[index] || {}) };
      for (const field of ["name", "vram_gb", "bandwidth_gbps", "cores"]) {
        if (!hwDirty["slot:" + index + ":" + field]) continue;
        const raw = String(slot[field] ?? "").trim();
        if (raw === "") delete entry[field];
        else entry[field] = field === "name" ? raw : Number(raw);
      }
      return entry;
    });
    for (const key of Object.keys(hwDirty)) {
      if (!hwDirty[key] || key.startsWith("slot:")) continue;
      const raw = String(hwForm[key] ?? "").trim();
      if (raw === "") {
        delete body[key];
        continue;
      }
      // These boxes are plain text, so anything typed is not guaranteed to be a
      // number. Only a real number becomes an override; other text keeps the
      // detected value instead of sending NaN.
      const num = Number(raw);
      if (Number.isFinite(num)) body[key] = num;
    }
    if (gpus.some((entry) => Object.keys(entry).length)) body.gpus = gpus;
    else delete body.gpus;
    return body;
  }

  // The count box and the add/remove buttons are the same control. Narrowing keeps
  // the largest-VRAM positions, so position 1 stays the card the fit model scores.
  function setSlotCount(count) {
    const target = Math.max(1, Math.min(8, Number(count) || 1));
    while (hwSlots.length < target) hwSlots.push({ name: "", vram_gb: "", bandwidth_gbps: "", cores: "" });
    hwSlots.length = target;
    hwForm.gpu_count = target;
    markHwField("gpu_count");
  }

  function removeSlot(index) {
    if (hwSlots.length <= 1) return;
    hwSlots.splice(index, 1);
    for (const key of Object.keys(hwDirty)) {
      if (key.startsWith("slot:") && Number(key.split(":")[1]) === index) delete hwDirty[key];
    }
    setSlotCount(hwSlots.length);
  }

  function markHwField(key) {
    hwDirty[key] = true;
    queueHardwareSave();
  }

  async function saveHardwareBar() {
    hwSaving = true;
    try {
      await saveSettings({ hardware: hardwareBody() });
      await loadHardware();
      if ((ui.models.query || "").trim()) loadSearch(1).catch(() => {});
    } catch (err) {
      toast(tr("settings.hardware") + ": " + err.message, "error");
    } finally {
      hwSaving = false;
    }
  }

  function queueHardwareSave() {
    clearTimeout(hwTimer);
    hwTimer = setTimeout(saveHardwareBar, 400);
  }

  const sorts = ["recommend", "downloads", "likes", "trendingScore", "lastModified"];
  const types = ["", "text-generation", "text-to-image", "image-to-text", "automatic-speech-recognition", "text-to-speech", "sentence-similarity"];
  const examples = [
    { label: "GGUF:", items: [["TheBloke/Mistral-7B-Instruct-v0.2-GGUF", "Mistral-7B-GGUF"], ["bartowski/Qwen2.5-7B-Instruct-GGUF", "Qwen2.5-7B-GGUF"]] },
    { label: "Diffusers:", items: [["stabilityai/stable-diffusion-xl-base-1.0", "SDXL-base"], ["runwayml/stable-diffusion-v1-5", "SD-v1.5"]] },
    { label: "LoRA/GPTQ:", items: [["predibase/glue_stsb", "LoRA-adapter"], ["TheBloke/Llama-2-7B-Chat-GPTQ", "Llama-2-GPTQ"]] },
    { label: "Datasets:", items: [["HuggingFaceFW/fineweb-edu", "FineWeb-Edu", true], ["OpenAssistant/oasst1", "OpenAssistant", true]] }
  ];
  function categoryTitle(category) {
    const key = "models.category." + category;
    const text = t(key, ui.lang);
    return text === key ? category.charAt(0).toUpperCase() + category.slice(1) : text;
  }

  const analysis = $derived(ui.models.analysis);
  const routeOptions = $derived(getConfiguredRouteOptions(ui.settings));
  let routeOverride = $state("");
  const routeKey = $derived(routeOverride || (analysis && !analysis.is_dataset ? routeKeyForAnalysis(analysis) : ""));

  function groupItems(items) {
    const categories = {};
    for (const item of items) {
      const category = item.category || "default";
      if (!categories[category]) categories[category] = [];
      categories[category].push(item);
    }
    return Object.entries(categories);
  }

  const dlMap = $derived(analysis ? dlStatusFor(analysis.repo) : {});
  const readmeHtml = $derived(ui.models.readmeHtml || renderMarkdown(ui.models.readmeMarkdown, ui.models.readmeBase));
  const selectedRepo = $derived(analysis ? analysis.repo : ui.models.repo);
  const hubSortFallbacks = { modelscope: { trendingScore: "downloads" } };
  // The dropdown lists the saved hub sources, so two Hugging Face mirrors can be
  // told apart instead of offering one option per API shape.
  const hubList = $derived(hubSources(ui.settings));

  let activeRow = $state("");
  let showRevisions = $state(false);
  let atTop = $state(true);
  let atBottom = $state(true);

  function hubSourceId() {
    // The selector lists the user's saved hub access sources, so it has to name the
    // entry the server is really using, not just the API shape of its source.
    return activeHubId(ui.settings);
  }

  function hubRepoPageURL(repo, isDataset) {
    const encoded = String(repo).split("/").map(encodeURIComponent).join("/");
    const template = (ui.settings.source_page_templates || {})[isDataset ? "dataset" : "model"] || "";
    if (template) return template.replace("{repo}", encoded);
    const base = ui.settings.endpoint || DEFAULT_HF_ENDPOINT;
    return base + "/" + (isDataset ? "datasets/" : "") + encoded;
  }

  function initials(repo) {
    const parts = String(repo || "").split("/");
    return (parts[0] || repo || "").substring(0, 2).toUpperCase();
  }

  async function selectRepo(repo, revision = "main") {
    try {
      activeRow = repo;
      const data = await loadAnalyze(repo);
      if (revision !== "main") data.branch = revision;
      loadReadme(repo, { revision, dataset: Boolean(data.is_dataset) }).catch(() => {});
    } catch (err) {
      toast(err.message || tr("common.error"), "error");
    }
  }

  async function downloadItem(item, filename = "") {
    if (!analysis) return;
    const filterValue = filename || item?.filter_value || item?.id || "";
    const dlRepo = item?.upstream_repo || analysis.repo;
    // An upstream companion always uses main: the analyzed revision applies only
    // to the repo being analyzed, not to its ancestors.
    const revision = dlRepo === analysis.repo ? (analysis.branch || "main") : "main";
    const body = { repo: dlRepo, revision, dataset: Boolean(analysis.is_dataset), filters: filterValue ? [filterValue] : [], exactMatch: Boolean(filterValue) };
    if (routeKey) body.routeKey = routeKey;
    if (item?.upstream_repo) body.localRepo = analysis.repo;
    try {
      if (!(await checkDownloadDiskSpace(body))) return;
      const result = await startDownload(body);
      if (result.message) {
        toast(result.message, "info");
        return;
      }
      setDlStatus(analysis.repo, filterValue, "queued");
      toast(tr("models.queued") + ": " + (item?.label || filterValue), "success");
    } catch (err) {
      toast(tr("common.failed") + ": " + err.message, "error");
    }
  }

  async function preview(item) {
    if (!analysis) return;
    const body = { repo: analysis.repo, revision: analysis.branch || "main", dataset: Boolean(analysis.is_dataset), filters: item && item.filter_value ? [item.filter_value] : [], exactMatch: Boolean(item && item.filter_value) };
    if (routeKey) body.routeKey = routeKey;
    try {
      const result = await previewPlan(body);
      toast(tr("models.plan") + ": " + (result.totalFiles || 0) + " files · " + (result.totalSizeHuman || formatBytes(result.totalSize || 0)));
    } catch (err) {
      toast(err.message || tr("common.error"), "error");
    }
  }

  function onScroll(event) {
    const scroller = event.currentTarget;
    const maxScroll = scroller.scrollHeight - scroller.clientHeight;
    atTop = scroller.scrollTop < 20;
    atBottom = maxScroll <= 0 || scroller.scrollTop >= maxScroll - 20;
  }

  let showAdvanced = $state(false);
  let advFilter = $state("");
  let advExclude = $state("");

  const splitFilter = (value) => String(value || "").split(",").map((part) => part.trim()).filter(Boolean);

  function clearAnalysis() {
    clearModelsDetail();
    routeOverride = "";
    activeRow = "";
  }

  // The hub selector sits in the header strip so the source can be changed where
  // the results are read. It mirrors the server's source, and a switch the server
  // rejected snaps back to it instead of leaving a value the server refused.
  let sourcePick = $state("");
  let sourceSaving = $state(false);

  $effect(() => {
    sourcePick = hubSourceId();
  });

  async function switchHubSource(next) {
    if (sourceSaving) return;
    const previous = hubSourceId();
    if (next === previous) return;
    sourceSaving = true;
    try {
      await useHubSource(next);
      // An analysis, its plan and its README belong to the hub they were fetched
      // from: revisions and resolve URLs do not exist on the other one.
      clearAnalysis();
      await loadSearch(1);
      toast(tf("models.sourceSwitched", ui.lang, { name: hubSourceLabel(hubList.find((entry) => entry.id === next), ui.settings) }), "success");
    } catch (err) {
      sourcePick = hubSourceId();
      toast(tr("common.failed") + ": " + err.message, "error");
    } finally {
      sourceSaving = false;
    }
  }

  function applyAdvancedOptions() {
    showAdvanced = false;
    toast(tr("models.optionsApplied"), "success");
  }

  async function downloadAll() {
    if (!analysis) return;
    const filters = splitFilter(advFilter);
    const body = {
      repo: analysis.repo,
      revision: analysis.branch || "main",
      dataset: Boolean(analysis.is_dataset),
      filters,
      excludes: splitFilter(advExclude),
      // Manual filters name specific quant/file segments, so keep matching
      // exact enough that q6_k does not also pull q6_k_xl.
      exactMatch: filters.length > 0
    };
    if (!analysis.is_dataset && routeKey) body.routeKey = routeKey;
    try {
      if (!(await checkDownloadDiskSpace(body))) return;
      await startDownload(body);
      toast(tr("models.downloadStarted") + ": " + analysis.repo, "success");
      ui.page = "jobs";
    } catch (err) {
      toast(tr("common.failed") + ": " + err.message, "error");
    }
  }

  let debounceTimer = null;
  let firstRun = true;

  // Legacy parity: entering the page lists the default results, typing debounces a
  // search, Enter analyzes a repo ID, and a filter change re-runs the search.
  // The effect reads only the query and the filters, never what loadSearch writes,
  // so a search cannot re-trigger itself.
  $effect(() => {
    const key = [ui.models.query.trim(), ui.models.sort, ui.models.type, ui.models.gguf, ui.models.datasets].join("|");
    if (firstRun) {
      firstRun = false;
      loadSearch(1).catch((err) => toast(err.message, "error"));
      return;
    }
    clearTimeout(debounceTimer);
    debounceTimer = setTimeout(() => { loadSearch(1).catch((err) => toast(err.message, "error")); }, 380);
  });

  // App.svelte swaps page components with {#if}, so navigating away destroys this
  // one while a debounced search or the hardware autosave may still be pending.
  // Their callbacks then run against state the user has left, and a failure
  // raises a toast on an unrelated page.
  onDestroy(() => {
    clearTimeout(hwTimer);
    clearTimeout(debounceTimer);
  });

  function submitQuery() {
    clearTimeout(debounceTimer);
    const value = ui.models.query.trim();
    if (value.includes("/")) { selectRepo(value); return; }
    loadSearch(1).catch((err) => toast(err.message, "error"));
  }

  $effect(() => { loadDiskFree().catch(() => {}); });
</script>

<div class="models-split">
  <div class="models-list-panel">
    <div class="models-list-header">
      <div class="models-search-wrap">
        <span class="models-search-icon"><Icon name="search" size={16} /></span>
        <input id="analyzeInput" class="models-search-input" type="text" placeholder={tr("models.placeholder")} autocomplete="off" spellcheck="false" bind:value={ui.models.query} onkeydown={(event) => { if (event.key === "Enter") submitQuery(); }} />
        {#if ui.models.busy}
          <span class="models-spinner"><Icon name="spinner" size={14} cls="spin" /></span>
        {/if}
        {#if ui.models.query}
          <button id="modelsSearchClear" class="models-search-clear" type="button" title={tr("common.clear")} aria-label={tr("common.clearSearch")} onclick={(event) => { ui.models.query = ""; ui.models.results = []; ui.models.nextUrl = ""; ui.models.warnings = []; event.currentTarget.focus(); }}>
            <Icon name="close" size={14} />
          </button>
        {/if}
      </div>

      <div class="models-filter-row">
        <select id="modelsSortSelect" class="models-select" aria-label={tr("common.sort")} bind:value={ui.models.sort}>
          {#each sorts as key}
            <option value={key} title={hubSortFallbacks[hubSourceId()]?.[key] ? tf("models.sortFallbackHint", ui.lang, { key, fallback: hubSortFallbacks[hubSourceId()][key] }) : ""}>{tr("models.sort." + key)}</option>
          {/each}
        </select>
        <select id="modelsTypeSelect" class="models-select" aria-label={tr("common.colType")} bind:value={ui.models.type}>
          <option value="">{tr("models.type.all")}</option>
          {#each types.filter(Boolean) as key}<option value={key}>{tr("models.type." + key)}</option>{/each}
        </select>
        <label class="checkbox-label checkbox-label-sm" title={tr("models.ggufTitle")}><input type="checkbox" id="modelsGguf" bind:checked={ui.models.gguf} /><span>GGUF</span></label>
        <label class="checkbox-label checkbox-label-sm" title={tr("models.datasetsTitle")}><input type="checkbox" id="modelsDatasets" bind:checked={ui.models.datasets} /><span>DS</span></label>
      </div>

      <div class="models-storage-strip">
        <div class="chip-pair">
          <span class={"chip context-chip" + (hwSources.context_length === "manual" ? " hw-field-manual" : "")} title={tr("settings.contextHint")}>
            <Icon name="clock" size={13} />
            <span class="chip-label">{tr("settings.context")}</span>
            <input
              class="hw-control hw-number"
              type="text"
              inputmode="numeric"
              value={hwForm.context_length}
              placeholder="8192"
              aria-label={tr("settings.context")}
              oninput={(event) => {
                hwForm.context_length = event.currentTarget.value;
                markHwField("context_length");
              }}
            />
          </span>
          <select
            id="modelsSourceSelect"
            class="models-select source-select"
            title={tr("settings.sourceHint")}
            aria-label={tr("settings.source")}
            bind:value={sourcePick}
            disabled={sourceSaving}
            onchange={(event) => switchHubSource(event.currentTarget.value)}
          >
            {#each hubList as entry}
              <option value={entry.id}>{hubSourceLabel(entry, ui.settings)}</option>
            {/each}
          </select>
        </div>
        <div class="hardware-bar" role="group" aria-label={tr("models.gpuBarName")} title={tr("settings.hardwareBarHint")}>
          <div class="hardware-bar-head">
            <span class="hardware-bar-name hw-head-name">
              <Icon name="gpu" size={14} />
              <span class="hw-field-label">{tr("models.gpuBarName")}</span>
            </span>
            <span class={"hw-gpu-spec hw-head-memory" + (hwSources.system_ram_gb === "manual" ? " hw-field-manual" : "")} title={tr("settings.ramHint")}>
              <Icon name="memory" size={13} />
              <span class="hw-field-label">{tr("settings.ramShort")}</span>
              <input
                class="hw-control hw-number"
                type="text"
                inputmode="numeric"
                value={hwForm.system_ram_gb}
                placeholder="?"
                aria-label={tr("settings.ram")}
                oninput={(event) => {
                  hwForm.system_ram_gb = event.currentTarget.value;
                  markHwField("system_ram_gb");
                }}
              />
              <span>G</span>
            </span>
            <button type="button" class="gpu-slot-add" title={tr("settings.addGpu")} aria-label={tr("settings.addGpu")} onclick={() => setSlotCount(hwSlots.length + 1)}>
              <Icon name="plus" size={12} />
            </button>
          </div>
          {#each hwSlots as slot, index (index)}
            <div class="hw-gpu{hwSources.gpu_name === "manual" && index === 0 ? " hw-field-manual" : ""}">
              <GpuCombobox
                value={slot.name}
                entries={gpuOptions}
                lang={ui.lang}
                label={tr("settings.gpu") + " " + (index + 1)}
                onpick={(name) => {
                  hwSlots[index].name = name;
                  markHwField("slot:" + index + ":name");
                }}
              />
              <span class="hw-gpu-spec" title={tr("settings.coresHint")}>
                {slot.vram_gb ? slot.vram_gb + " GB" : "?"} · {slot.bandwidth_gbps ? slot.bandwidth_gbps + " GB/s" : "?"} · {coreCount(slot.cores)}
              </span>
              {#if hwSlots.length > 1}
                <button type="button" class="gpu-slot-remove" title={tr("settings.removeGpu")} aria-label={tr("settings.removeGpu")} onclick={() => removeSlot(index)}>
                  <Icon name="close" size={12} />
                </button>
              {/if}
            </div>
          {/each}
          {#if hwSaving}<span class="hw-state spinner-sm"><Icon name="spinner" size={13} /></span>{/if}
        </div>
      </div>
    </div>

    {#if ui.models.warnings.length}
      <div class="models-search-warnings" role="status" id="modelsSearchWarnings">
        {#each ui.models.warnings as warning (warning)}
          <p class="form-hint">{warning}</p>
        {/each}
      </div>
    {/if}

    <div id="modelsResultList" class="models-result-list">
      {#if ui.models.results.length === 0}
        <div class="models-list-hint">{ui.models.query ? tf("models.noResultsFor", ui.lang, { query: ui.models.query }) : tr("models.listHint")}</div>
      {:else}
        {#each ui.models.results as row (row.id)}
          {@const caps = detectSearchCapabilities(row)}
          {@const parts = String(row.id || "").split("/")}
          {@const author = parts.length > 1 ? parts[0] : ""}
          {@const name = parts.length > 1 ? parts.slice(1).join("/") : row.id}
          <div class="model-row{activeRow === row.id ? " model-row-active" : ""}" role="button" tabindex="0" data-repo={row.id} onclick={() => selectRepo(row.id)} onkeydown={(event) => { if (event.key === "Enter") selectRepo(row.id); }}>
            <div class="model-row-avatar">
              {#if row.avatar}<img class="model-row-avatar-img" src={row.avatar} alt="" loading="lazy" onerror={(event) => event.currentTarget.remove()} />{/if}
              {initials(author || name)}
            </div>
            <div class="model-row-info">
              <div class="model-row-name">{name}</div>
              <div class="model-row-meta">
                {author}
                {#if row.downloads}<span class="model-row-stat">↓{formatNumber(row.downloads)}</span>{/if}
                {#if row.likes}<span class="model-row-stat like-accent">♥{formatNumber(row.likes)}</span>{/if}
                {#if row.gated}<span class="model-row-stat" title={tr("models.gated")} aria-label={tr("models.gated")}><Icon name="lock" size={12} /></span>{/if}
              </div>
            </div>
            <div class="model-row-right">
              <CapabilityIcons capabilities={caps.capabilities} titles={caps.titles} />
              {#if row.fit}
                <span class="quant-grade model-row-grade quant-grade-{gradeClass(row.fit.grade)}" title={fitTitle(row.fit, tr)}>{row.fit.grade || "?"}</span>
              {/if}
              {#if row.cached}<span class="model-row-badge model-row-badge-local" title={row.cacheSource || tr("models.localCache")}>{tr("models.localBadge")}</span>{/if}
              {#if (row.tags || []).includes("gguf")}<span class="model-row-badge model-row-badge-gguf">GGUF</span>{/if}
            </div>
          </div>
        {/each}
        {#if ui.models.nextUrl}
          <button type="button" class="example-btn models-load-more" title={tr("models.fetchNextHint")} onclick={() => loadSearch(ui.models.page + 1).catch((err) => toast(err.message, "error"))}>{tr("models.loadMore")}</button>
        {/if}
      {/if}
    </div>
  </div>

  <div class="models-list-resize-handle" aria-hidden="true"></div>

  <div class="models-detail-panel">
    {#if analysis}
      <div id="modelsDetailHeader" class="models-detail-header">
        <div class="models-detail-title-wrap">
          <span class="models-detail-repo">{analysis.repo}</span>
        </div>
        <div class="models-detail-header-right">
          <a href={hubRepoPageURL(analysis.repo, Boolean(analysis.is_dataset))} target="_blank" rel="noopener" class="btn btn-secondary btn-xs">{tr("models.openHub")} ↗</a>
          <button id="analyzeBtn" class="btn btn-ghost btn-xs models-reanalyze-btn" title={tr("models.refreshModel")} aria-label={tr("models.refreshModel")} onclick={() => selectRepo(analysis.repo)}>
            <Icon name="refresh" size={14} />
          </button>
        </div>
      </div>
    {/if}

    <button class="scroll-arrow scroll-arrow-up{atTop ? "" : " visible"}" type="button" title={tr("common.scrollToTop")} aria-label={tr("common.scrollToTop")} onclick={() => document.querySelector("#analyzeResult")?.scrollTo({ top: 0, behavior: "smooth" })}>
      <Icon name="chevronUp" size={18} />
    </button>
    <button class="scroll-arrow scroll-arrow-down{atBottom ? "" : " visible"}" type="button" title={tr("common.scrollToBottom")} aria-label={tr("common.scrollToBottom")} onclick={() => { const scroller = document.querySelector("#analyzeResult"); scroller?.scrollTo({ top: scroller.scrollHeight, behavior: "smooth" }); }}>
      <Icon name="chevronDown" size={18} />
    </button>

    <div id="analyzeResult" class="models-analyze-result" onscroll={onScroll}>
      {#if !analysis}
        <div class="empty-state">
          <div class="empty-icon"><Icon name="cube" size={64} /></div>
          <h3>{tr("models.emptyTitle")}</h3>
          <p>{tr("models.emptyHint")}</p>
          {#each examples as group}
            <div class="example-repos">
              <span class="example-label">{group.label}</span>
              {#each group.items as [repo, label, isDataset]}
                <button class="example-btn" type="button" data-repo={repo} onclick={() => selectRepo(repo)}>{label}</button>
              {/each}
            </div>
          {/each}
        </div>
      {:else}
        <div class="analysis-card">
          <div class="analysis-header">
            <div class="analysis-repo">
              {analysis.repo}
              {#if analysis.branch && analysis.branch !== "main"}
                <span class="analysis-branch" title={tf("models.revisionTitle", ui.lang, { revision: analysis.branch })}><Icon name="branch" size={14} /> {analysis.branch}</span>
              {/if}
              {#if (analysis.refs || []).length > 1}
                <button class="btn-link" type="button" onclick={() => (showRevisions = true)}>{tr("models.changeRevision")}</button>
              {/if}
            </div>
            <span class="analysis-type">{analysis.type_description || analysis.type}</span>
            <div class="analysis-meta">
              <span>{tf("cache.cardFiles", ui.lang, { n: analysis.file_count })}</span>
              <span>{analysis.total_size_human}</span>
            </div>
          </div>

          <div class="analysis-body">
            {#if analysis.transformers}
              <div class="analysis-section">
                <h4>{tr("models.sectionModelConfig")}</h4>
                <div class="analysis-grid">
                  {#if analysis.transformers.architecture}<div class="analysis-stat"><div class="analysis-stat-label">{tr("models.architecture")}</div><div class="analysis-stat-value">{analysis.transformers.architecture}</div></div>{/if}
                  {#if analysis.transformers.estimated_parameters}<div class="analysis-stat"><div class="analysis-stat-label">{tr("models.parameters")}</div><div class="analysis-stat-value">~{analysis.transformers.estimated_parameters}</div></div>{/if}
                  {#if analysis.transformers.hidden_size}<div class="analysis-stat"><div class="analysis-stat-label">{tr("models.hiddenSize")}</div><div class="analysis-stat-value">{analysis.transformers.hidden_size}</div></div>{/if}
                  {#if analysis.transformers.num_hidden_layers}<div class="analysis-stat"><div class="analysis-stat-label">{tr("models.layers")}</div><div class="analysis-stat-value">{analysis.transformers.num_hidden_layers}</div></div>{/if}
                  {#if analysis.transformers.context_length}<div class="analysis-stat"><div class="analysis-stat-label">{tr("models.contextLength")}</div><div class="analysis-stat-value">{Number(analysis.transformers.context_length).toLocaleString()} tokens</div></div>{/if}
                  {#if analysis.transformers.precision}<div class="analysis-stat"><div class="analysis-stat-label">{tr("models.precision")}</div><div class="analysis-stat-value">{analysis.transformers.precision}</div></div>{/if}
                </div>
              </div>
            {/if}

            {#if analysis.gguf}
              <div class="analysis-section">
                <h4>{tr("models.sectionGguf")}</h4>
                <div class="analysis-grid">
                  {#if analysis.gguf.model_name}<div class="analysis-stat"><div class="analysis-stat-label">{tr("models.model")}</div><div class="analysis-stat-value">{analysis.gguf.model_name}</div></div>{/if}
                  {#if analysis.gguf.parameter_count}<div class="analysis-stat"><div class="analysis-stat-label">{tr("models.parameters")}</div><div class="analysis-stat-value">{analysis.gguf.parameter_count}</div></div>{/if}
                </div>
                {#if (analysis.gguf.model_chain || []).length > 1}
                  <div style="margin-top:12px">
                    <div class="analysis-stat-label" style="margin-bottom:6px">{tr("models.lineage")}</div>
                    {#each analysis.gguf.model_chain as node, index}
                      {@const relLabel = node.is_current ? (node.relation ? node.relation + " · " + tr("models.thisModel") : tr("models.thisModel")) : index === 0 ? tr("models.baseModel") : (node.relation || "")}
                      <div style={`display:flex;align-items:center;gap:5px;padding:2px 0;padding-left:${index * 14}px`}>
                        {#if index > 0}<span style="color:var(--color-text-muted);font-size:11px;opacity:.6;flex-shrink:0">└─</span>{/if}
                        <span style="font-size:11px;opacity:.7;flex-shrink:0">{node.is_current ? "◉" : index === 0 ? "◈" : "⚙"}</span>
                        <span style={node.is_current ? "font-size:11px;font-family:var(--font-mono);color:var(--color-text);font-weight:600;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;min-width:0" : "font-size:11px;font-family:var(--font-mono);color:var(--color-text-secondary);overflow:hidden;text-overflow:ellipsis;white-space:nowrap;min-width:0"}>{node.repo}</span>
                        {#if relLabel}<span style={node.is_current ? "font-size:10px;color:var(--color-info);flex-shrink:0;margin-left:2px" : "font-size:10px;color:var(--color-text-muted);flex-shrink:0;margin-left:2px"}>{relLabel}</span>{/if}
                      </div>
                    {/each}
                  </div>   {/if}
              </div>
            {/if}

            {#if analysis.diffusers}
              <div class="analysis-section">
                <h4>{tr("models.sectionDiffusers")}</h4>
                <div class="analysis-grid">
                  {#if analysis.diffusers.pipeline_type}<div class="analysis-stat"><div class="analysis-stat-label">{tr("models.pipeline")}</div><div class="analysis-stat-value">{analysis.diffusers.pipeline_type}</div></div>{/if}
                  {#if analysis.diffusers.diffusers_version}<div class="analysis-stat"><div class="analysis-stat-label">{tr("models.version")}</div><div class="analysis-stat-value">{analysis.diffusers.diffusers_version}</div></div>{/if}
                  {#if (analysis.diffusers.variants || []).length}<div class="analysis-stat"><div class="analysis-stat-label">{tr("models.variants")}</div><div class="analysis-stat-value">{analysis.diffusers.variants.join(", ")}</div></div>{/if}
                </div>
              </div>
            {/if}

            {#if analysis.dataset}
              <div class="analysis-section">
                <h4>{tr("models.sectionDataset")}</h4>
                <div class="analysis-grid">
                  {#if analysis.dataset.primary_format}<div class="analysis-stat"><div class="analysis-stat-label">{tr("models.format")}</div><div class="analysis-stat-value">{analysis.dataset.primary_format}</div></div>{/if}
                  {#if (analysis.dataset.configs || []).length}<div class="analysis-stat"><div class="analysis-stat-label">{tr("models.configs")}</div><div class="analysis-stat-value">{analysis.dataset.configs.join(", ")}</div></div>{/if}
                </div>
              </div>
            {/if}

            {#if analysis.lora}
              <div class="analysis-section">
                <h4>{tr("models.sectionLora")}</h4>
                <div class="analysis-grid">
                  {#if analysis.lora.adapter_type}<div class="analysis-stat"><div class="analysis-stat-label">{tr("models.adapterType")}</div><div class="analysis-stat-value">{analysis.lora.adapter_type}</div></div>{/if}
                  {#if analysis.lora.rank}<div class="analysis-stat"><div class="analysis-stat-label">{tr("models.rank")}</div><div class="analysis-stat-value">{analysis.lora.rank}</div></div>{/if}
                  {#if analysis.lora.alpha}<div class="analysis-stat"><div class="analysis-stat-label">{tr("models.alpha")}</div><div class="analysis-stat-value">{analysis.lora.alpha}</div></div>{/if}
                  {#if analysis.lora.base_model}<div class="analysis-stat"><div class="analysis-stat-label">{tr("models.baseModel")}</div><div class="analysis-stat-value">{analysis.lora.base_model}</div></div>{/if}
                </div>
              </div>
            {/if}

            {#if analysis.quantized}
              <div class="analysis-section">
                <h4>{tr("models.sectionQuant")}</h4>
                <div class="analysis-grid">
                  {#if analysis.quantized.method}<div class="analysis-stat"><div class="analysis-stat-label">{tr("models.method")}</div><div class="analysis-stat-value">{String(analysis.quantized.method).toUpperCase()}</div></div>{/if}
                  {#if analysis.quantized.bits}<div class="analysis-stat"><div class="analysis-stat-label">{tr("models.bits")}</div><div class="analysis-stat-value">{analysis.quantized.bits}-bit</div></div>{/if}
                  {#if analysis.quantized.group_size}<div class="analysis-stat"><div class="analysis-stat-label">{tr("models.groupSize")}</div><div class="analysis-stat-value">{analysis.quantized.group_size}</div></div>{/if}
                  {#if (analysis.quantized.backends || []).length}<div class="analysis-stat"><div class="analysis-stat-label">{tr("models.backends")}</div><div class="analysis-stat-value">{analysis.quantized.backends.slice(0, 3).join(", ")}</div></div>{/if}
                </div>
              </div>
            {/if}

            {#if (analysis.selectable_items || []).length}
              <div class="analysis-section">
                <div class="selectable-items quant-list" id="selectableItems">
                  {#if routeOptions.length && !analysis.is_dataset}
                    <div class="quant-category">
                      <div class="quant-category-title">{tr("models.saveTo")}</div>
                      <div class="quant-route-control">
                        <select class="search-select" id="quantRouteSelect" style="display:block; width:100%; max-width:320px; margin:4px 0 0 12px" aria-label={tr("models.routeFolderLabel")} value={routeKey} onchange={(event) => (routeOverride = event.target.value)}>
                          <option value="">{tr("models.serverDefault")}</option>
                          {#each routeOptions as option}<option value={option.key}>{option.label}</option>{/each}
                        </select>
                        <p class="form-hint" style="margin-top:4px">{tr("models.routeHint")}</p>
                      </div>
                    </div>
                  {/if}
                  {#each groupItems(analysis.selectable_items) as [category, items]}
                    <div class="quant-category">
                      <div class="quant-category-title">{categoryTitle(category)}</div>
                      {#each items as item (item.id || item.filter_value)}
                        {@const status = dlMap[item.filter_value || item.id] || ""}
                        <div class="quant-row{item.recommended ? " quant-row-rec" : ""}" data-fv={item.filter_value || item.id}>
                          {#if status === "done"}
                            <span class="quant-dl-btn quant-dl-btn-done" title={tr("models.alreadyDownloaded")}>✓</span>
                          {:else if status === "queued"}
                            <span class="quant-dl-btn quant-dl-btn-queued" title={tr("models.queued")}>…</span>
                          {:else if status === "running"}
                            <span class="quant-dl-btn quant-dl-btn-running" title={tr("models.downloading")}><span class="quant-spinner"></span></span>
                          {:else}
                            <button class="quant-dl-btn" type="button" title={tr("models.downloadQuant")} onclick={() => downloadItem(item)}><Icon name="download" size={14} /></button>
                          {/if}
                          <span class="quant-label">{item.label}</span>
                          {#if item.recommended}<span class="quant-rec">★ {tr("models.recBadge")}</span>{/if}
                          {#if item.fit && item.fit.grade}
                            <span class="quant-grade quant-grade-{gradeClass(item.fit.grade)}" title={item.fit.reason || ""}>{item.fit.grade}{item.fit.est_tokens_per_sec ? " · " + item.fit.est_tokens_per_sec + " tok/s" : ""}</span>
                          {/if}
                          {#if item.size_human}<span class="quant-size">{item.size_human}</span>{/if}
                          {#if item.fit}
                            <span class="quant-ram quant-vram" title={item.fit.kv_source === "config" ? tr("models.kvFromConfig") : tr("models.kvFlatEstimate")}>{item.fit.needed_gb} GB VRAM</span>
                          {:else if item.ram_human}
                            <span class="quant-ram">{item.ram_human} RAM</span>
                          {/if}
                          {#if item.quality >= 1 && item.quality <= 5}
                            <span class="selector-stars">{"★".repeat(item.quality)}{"☆".repeat(5 - item.quality)}</span>
                          {/if}
                          {#if item.description}<span class="quant-desc">{@html escapeHtml(item.description).replace(/\brecommended\b/gi, '<span class="quant-desc-rec">$&</span>')}</span>{/if}
                        </div>
                      {/each}
                    </div>
                  {/each}
                </div>
              </div>
            {/if}

            {#if (analysis.related_downloads || []).length}
              <div class="analysis-section related-downloads-section">
                <h4>{tr("models.related")}</h4>
                <div class="related-downloads">
                  {#each analysis.related_downloads as download (download.repo + "|" + download.label)}
                    <div class="related-download-card{download.required ? " required" : ""}">
                      <div class="related-download-info">
                        <span class="related-download-label">{download.label}{#if download.required}<span class="badge-required">{tr("models.required")}</span>{/if}</span>
                        <span class="related-download-repo">{download.repo}</span>
                        {#if download.description}<span class="related-download-desc">{download.description}</span>{/if}
                        {#if download.size_human}<span class="related-download-size">{download.size_human}</span>{/if}
                      </div>
                      <button class="btn btn-secondary btn-sm" type="button" onclick={() => selectRepo(download.repo)}>{tr("models.analyze")}</button>
                    </div>
                  {/each}
                </div>
              </div>
            {/if}

            {#if !(analysis.selectable_items || []).length && (analysis.files || []).length}
              {@const files = analysis.files.filter((file) => { const name = (file.path || file.name || "").toLowerCase(); return (!name.endsWith(".md") && !name.endsWith(".gitattributes") && !name.endsWith(".txt")) || analysis.files.length <= 5; })}
              {#if files.length}
                <div class="analysis-section">
                  {#if routeOptions.length && !analysis.is_dataset}
                    <div class="quant-category">
                      <div class="quant-category-title">{tr("models.saveTo")}</div>
                      <div class="quant-route-control">
                        <select class="search-select" id="nonSelectableRoute" style="display:block; width:100%; max-width:320px; margin:4px 0 0 12px" aria-label={tr("models.routeFolderLabel")} value={routeKey} onchange={(event) => (routeOverride = event.target.value)}>
                          <option value="">{tr("models.serverDefault")}</option>
                          {#each routeOptions as option}<option value={option.key}>{option.label}</option>{/each}
                        </select>
                        <p class="form-hint" style="margin-top:4px">{tr("models.routeHint")}</p>
                      </div>
                    </div>
                  {/if}
                  <div class="quant-category">
                    <div class="quant-category-title">{tr("cache.statFiles")} ({analysis.files.length})</div>
                    {#each files.slice(0, 50) as file (file.path || file.name)}
                      <div class="quant-row">
                        <button class="quant-dl-btn" type="button" title={tr("models.downloadFile")} onclick={() => downloadItem(null, file.path || file.name)}><Icon name="download" size={14} /></button>
                        <span class="quant-label" style="font-family:var(--font-mono);font-size:12px">{file.path || file.name}</span>
                        {#if file.size_human || file.size}<span class="quant-size">{file.size_human || formatBytes(file.size)}</span>{/if}
                      </div>
                    {/each}
                    {#if analysis.files.length > 50}<div class="quant-row" style="color:var(--color-text-muted);font-size:12px;padding:6px 12px">{tf("cache.moreFiles", ui.lang, { n: analysis.files.length - 50 })}</div>{/if}
                  </div>
                </div>
              {/if}
            {/if}

            <div class="analysis-section readme-section" hidden={!readmeHtml}>
              <h4>{tr("models.readme")}</h4>
              <div class="readme-preview">{@html readmeHtml}</div>
            </div>
          </div>
          <div class="analysis-actions-wrapper">
            <div class="analysis-actions">
              <button class="btn btn-ghost" type="button" onclick={clearAnalysis}>{tr("models.clear")}</button>
              {#if !(analysis.selectable_items || []).length}
                <button class="btn btn-ghost" type="button" onclick={() => (showAdvanced = true)}>{tr("models.options")}</button>
                <button class="btn btn-primary" type="button" onclick={downloadAll}>{tr("models.downloadAll")}</button>
              {/if}
            </div>
          </div>
        </div>
      {/if}
    </div>
  </div>
</div>

<Modal title={tr("models.selectRevision")} open={showRevisions} onclose={() => (showRevisions = false)}>
  <p style="margin-bottom: 16px; color: var(--color-text-secondary);">{tr("models.selectRevisionHint")}</p>
  {@const refs = analysis?.refs || []}
  {@const branches = refs.filter((ref) => ref.type === "branch")}
  {@const tags = refs.filter((ref) => ref.type === "tag")}
  {#if branches.length}
    <div class="ref-group">
      <h5>{tr("models.branches")}</h5>
      <div class="ref-list">
        {#each branches as ref (ref.name)}
          <button class="ref-btn{ref.name === "main" ? " ref-default" : ""}" type="button" onclick={() => { showRevisions = false; selectRepo(analysis.repo, ref.name); }}>
            <Icon name="branch" size={16} /> {ref.name}
            {#if ref.name === "main"}<span class="ref-badge">{tr("models.default")}</span>{/if}
          </button>
        {/each}
      </div>
    </div>
  {/if}
  {#if tags.length}
    <div class="ref-group">
      <h5>{tr("models.tags")}</h5>
      <div class="ref-list">
        {#each tags.slice(0, 10) as ref (ref.name)}
          <button class="ref-btn" type="button" onclick={() => { showRevisions = false; selectRepo(analysis.repo, ref.name); }}>
            <Icon name="tag" size={16} /> {ref.name}
          </button>
        {/each}
        {#if tags.length > 10}<div class="ref-more">{tf("models.moreTags", ui.lang, { n: tags.length - 10 })}</div>{/if}
      </div>
    </div>
  {/if}
  <div class="form-actions" style="margin-top: 20px;">
    <button class="btn btn-ghost" type="button" onclick={() => { showRevisions = false; selectRepo(analysis.repo, "main"); }}>{tr("models.useDefault")}</button>
  </div>
</Modal>
<Modal title={tr("models.options")} open={showAdvanced} onclose={() => (showAdvanced = false)}>
  <div class="form-group">
    <label for="advFilter">{tr("models.fileFilter")}</label>
    <input id="advFilter" type="text" spellcheck="false" placeholder={tr("models.fileFilterPlaceholder")} bind:value={advFilter} />
    <p class="form-hint">{tr("models.fileFilterHint")}</p>
  </div>
  <div class="form-group">
    <label for="advExclude">{tr("models.excludeFilter")}</label>
    <input id="advExclude" type="text" spellcheck="false" placeholder={tr("models.excludeFilterPlaceholder")} bind:value={advExclude} />
    <p class="form-hint">{tr("models.excludeFilterHint")}</p>
  </div>
  <div class="form-actions">
    <button class="btn btn-secondary" type="button" onclick={() => (showAdvanced = false)}>{tr("common.cancel")}</button>
    <button class="btn btn-primary" type="button" onclick={applyAdvancedOptions}>{tr("models.apply")}</button>
  </div>
</Modal>
