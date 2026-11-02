<script>
  import { ui, loadJobs, loadSettings, saveSettings, toast } from "../store.svelte.js";
  import { t } from "../i18n.js";
  import { maxSpeedToMbps, mbpsToMaxSpeedStr } from "../format.js";
  import { ROUTE_LABELS } from "../routes.js";
  import JobList from "../components/JobList.svelte";
  import Stepper from "../components/Stepper.svelte";
  import Modal from "../components/Modal.svelte";
  import FolderBrowser from "../components/FolderBrowser.svelte";

  const tr = (key) => t(key, ui.lang);
  const presets = [5, 15, 25, 50, 0];
  const currentMbps = $derived(maxSpeedToMbps(ui.settings.maxSpeed));
  const jobs = $derived([...ui.jobs.values()]);
  // The gauge is the sum of the live per-job readings. A finalizing job is local
  // I/O with a zeroed reading and a paused job keeps its last reading, so only a
  // transferring job may contribute - otherwise a paused job inflates a gauge
  // that is supposed to answer "what is moving right now".
  const gauge = $derived(jobs.reduce((sum, job) => (job.status === "running" && job.phase !== "finalizing" ? sum + (job.progress?.bytesPerSecond || 0) : sum), 0));
  const gaugeMbps = $derived(gauge / 1024 / 1024);
  const counts = $derived.by(() => {
    const tally = { running: 0, queued: 0, paused: 0 };
    for (const job of jobs) {
      if (job.status === "running") tally.running += 1;
      else if (job.status === "queued") tally.queued += 1;
      else if (job.status === "paused") tally.paused += 1;
    }
    return tally;
  });
  // The page used to look dead when every job was paused: no speed, no ETA, an
  // empty-looking bar. The tally says what the jobs are doing instead.
  const summary = $derived([
    `${tr("jobs.status.running")} ${counts.running}`,
    `${tr("jobs.status.queued")} ${counts.queued}`,
    `${tr("jobs.status.paused")} ${counts.paused}`
  ].join(" - "));

  // The transport knobs moved here from the Settings page: they are the settings
  // you reach for while a download is already running, and the server applies
  // each one to running jobs, so leaving the page that shows the jobs to change
  // them was a detour. The mirror below is optimistic so a click shows up before
  // the round trip ends; a rejected write reloads the settings and the control
  // snaps back to what the server actually has.
  // maxStreamsPerHost is separate from the hidden `connections` setting (config file
  // or API only, no control here): the first is how many streams the whole process
  // may read from one host at once, the second is how many streams one big file
  // splits itself into. 0 means "follow connections", which is what the gate did
  // before the two ideas were split apart.
  let transport = $state({ maxActive: 3, maxStreamsPerHost: 0, retries: 4, verify: "size" });
  $effect(() => {
    const settings = ui.settings;
    if (!settings) return;
    transport = {
      maxActive: settings.maxActive ?? 3,
      maxStreamsPerHost: settings.maxStreamsPerHost ?? 0,
      retries: settings.retries ?? 4,
      verify: settings.verify || "size"
    };
  });

  async function setLimit(mbps) {
    try {
      await saveSettings({ maxSpeed: mbpsToMaxSpeedStr(mbps) });
    } catch (err) {
      toast(err.message || tr("common.error"), "error");
    }
  }

  // An empty field is a half-typed number, not a value: the stepper restores its
  // default on blur, so nothing is posted until a number arrives.
  async function applyTransport(field, value, fallback) {
    if (value === "") {
      transport[field] = "";
      return;
    }
    const number = parseInt(value);
    if (Number.isNaN(number)) {
      transport[field] = fallback;
      return;
    }
    if (number === transport[field]) return;
    transport[field] = number;
    try {
      await saveSettings({ [field]: number });
    } catch (err) {
      toast(err.message || tr("common.error"), "error");
      await loadSettings().catch(() => {});
    }
  }

  async function applyVerify(mode) {
    if (mode === transport.verify) return;
    transport.verify = mode;
    try {
      await saveSettings({ verify: mode });
    } catch (err) {
      toast(err.message || tr("common.error"), "error");
      await loadSettings().catch(() => {});
    }
  }

  // The download target moved here too: "where will this file land" is asked
  // while a job is being started, not while a form is being filled. The server
  // has no layout flag - a non-empty localDir IS local mode - so the select
  // posts that one field, and switching to local mode without a folder only
  // opens the folder field instead of writing an empty target.
  let storage = $state({ layout: "cache", localDir: "", routes: {}, presetRoot: "" });
  let picker = $state({ open: false, path: "", apply: null });
  $effect(() => {
    const settings = ui.settings;
    if (!settings) return;
    storage.layout = settings.storageMode || "cache";
    storage.localDir = settings.localDir || "";
    storage.routes = { ...(settings.downloadRoutes || {}) };
  });

  // The readout answers "where does a new download go right now": the Hub cache
  // the server resolved, or the local folder local mode writes into.
  const destination = $derived(storage.layout === "local" ? String(storage.localDir || "").trim() : String(ui.settings?.effectiveHubDir || "").trim());

  async function applyLayout(layout) {
    if (layout === storage.layout) return;
    storage.layout = layout;
    const localDir = String(storage.localDir || "").trim();
    if (layout === "local" && !localDir) {
      toast(tr("jobs.localDirRequired"), "info");
      return;
    }
    try {
      await saveSettings({ localDir: layout === "local" ? localDir : "" });
    } catch (err) {
      toast(err.message || tr("common.error"), "error");
      await loadSettings().catch(() => {});
    }
  }

  // Committing the folder is what switches the server into local mode, so an
  // empty or unchanged field posts nothing rather than clearing the layout.
  async function applyLocalDir() {
    if (storage.layout !== "local") return;
    const localDir = String(storage.localDir || "").trim();
    if (!localDir || localDir === String(ui.settings?.localDir || "").trim()) return;
    try {
      await saveSettings({ localDir });
    } catch (err) {
      toast(err.message || tr("common.error"), "error");
      await loadSettings().catch(() => {});
    }
  }

  // downloadRoutes replaces the whole map server-side, so the body is the loaded
  // map with the labeled fields merged over it: routes this page does not show
  // survive, and a cleared field removes its route.
  function routeBody() {
    const merged = { ...(ui.settings?.downloadRoutes || {}) };
    for (const route of ROUTE_LABELS) {
      const value = String(storage.routes[route.key] || "").trim();
      if (value) merged[route.key] = value;
      else delete merged[route.key];
    }
    return merged;
  }

  async function applyRoutes() {
    try {
      await saveSettings({ downloadRoutes: routeBody() });
    } catch (err) {
      toast(err.message || tr("common.error"), "error");
      await loadSettings().catch(() => {});
    }
  }

  function fillRoutePreset() {
    const root = String(storage.presetRoot || "").trim();
    if (!root) {
      toast(tr("settings.presetRootHint"), "error");
      return;
    }
    const separator = root.includes("\\") && !root.includes("/") ? "\\" : "/";
    const cleanRoot = root.replace(/[\\/]+$/, "");
    for (const route of ROUTE_LABELS) storage.routes[route.key] = [cleanRoot, ...route.preset].join(separator);
    applyRoutes();
  }

  // A browser cannot return an absolute local path, so the picker lists folders
  // through the server and applies the picked path to one field.
  function openPicker(start, apply) {
    picker.open = true;
    picker.path = String(start || "").trim();
    picker.apply = apply;
  }
</script>

<header class="page-header">
  <div>
    <h1>{tr("nav.jobs")}</h1>
    <p class="page-subtitle"><span class="live-dot{ui.connected ? "" : " offline"}"></span> {ui.connected ? tr("jobs.subtitle") : tr("common.reconnecting")}</p>
  </div>
  <div class="page-actions">
    <button class="btn btn-secondary btn-sm" type="button" onclick={() => loadJobs().catch((err) => toast(err.message, "error"))}>{tr("common.refresh")}</button>
  </div>
</header>

<div class="speed-bar">
  <div class="speed-row" title={tr("jobs.speedLimitHint")}>
    <span class="speed-label">{tr("jobs.speedLimit")}</span>
    <div class="speed-presets">
      {#each presets as mbps}
        <button type="button" class="speed-btn{currentMbps === mbps ? " active" : ""}" onclick={() => setLimit(mbps)}>{mbps === 0 ? "∞" : mbps}</button>
      {/each}
    </div>
    <div class="speed-divider"></div>
    <div class="speed-input">
      <input type="text" inputmode="decimal" id="maxSpeedJobs" placeholder="∞" value={currentMbps ? String(Math.round(currentMbps * 10) / 10) : ""} onkeydown={(event) => { if (event.key === "Enter") setLimit(Number(event.target.value) || 0); }} />
      <span class="speed-unit">MB/s</span>
    </div>
    <span class="speed-current">{tr("jobs.currentSpeed")}: <strong>{gaugeMbps.toFixed(1) + (currentMbps > 0 ? " / " + currentMbps.toFixed(1) : "")} MB/s</strong></span>
    <span class="speed-counters">{summary}</span>
  </div>
  <div class="transport-row">
    <span class="speed-label" title={tr("jobs.transportHint")}>{tr("settings.transfer")}</span>
    <div class="transport-field">
      <label class="transport-label" for="maxStreamsJobs" title={tr("settings.maxStreamsHint")}>{tr("settings.maxStreamsPerHost")}</label>
      <Stepper id="maxStreamsJobs" value={transport.maxStreamsPerHost} min="0" max="64" defaultValue="0" label={tr("settings.maxStreamsPerHost")} onchange={(value) => applyTransport("maxStreamsPerHost", value, 0)} />
    </div>
    <div class="transport-field">
      <label class="transport-label" for="maxActiveJobs">{tr("settings.concurrent")}</label>
      <Stepper id="maxActiveJobs" value={transport.maxActive} min="1" max="10" defaultValue="3" label={tr("settings.concurrent")} onchange={(value) => applyTransport("maxActive", value, 3)} />
    </div>
    <div class="transport-field">
      <label class="transport-label" for="retriesJobs">{tr("settings.retries")}</label>
      <Stepper id="retriesJobs" value={transport.retries} min="0" max="20" defaultValue="4" label={tr("settings.retries")} onchange={(value) => applyTransport("retries", value, 4)} />
    </div>
    <div class="transport-field">
      <label class="transport-label" for="verifyJobs" title={tr("settings.verifyHint")}>{tr("settings.verify")}</label>
      <select id="verifyJobs" class="transport-select" value={transport.verify} onchange={(event) => applyVerify(event.target.value)}>
        <option value="size">{tr("settings.verifySizeOnly")}</option>
        <option value="sha256">SHA-256</option>
        <option value="none">{tr("settings.verifyNone")}</option>
      </select>
    </div>
  </div>
  <div class="storage-row">
    <span class="speed-label" title={tr("jobs.downloadToHint")}>{tr("jobs.downloadTo")}</span>
    <div class="transport-field">
      <label class="transport-label" for="downloadLayoutJobs" title={tr("settings.layoutHint")}>{tr("settings.layout")}</label>
      <select id="downloadLayoutJobs" class="transport-select" value={storage.layout} onchange={(event) => applyLayout(event.target.value)}>
        <option value="cache">{tr("settings.layoutCache")}</option>
        <option value="local">{tr("settings.layoutLocal")}</option>
      </select>
    </div>
    {#if storage.layout === "local"}
      <div class="transport-field storage-dir-field" id="localDirGroup">
        <label class="transport-label" for="localDirJobs" title={tr("settings.localDirHint")}>{tr("settings.localDir")}</label>
        <div class="input-with-browse">
          <input type="text" id="localDirJobs" class="path-input" placeholder="D:\Models" spellcheck="false" bind:value={storage.localDir} onchange={applyLocalDir} />
          <button type="button" class="btn btn-secondary btn-sm" onclick={() => openPicker(storage.localDir, (picked) => { storage.localDir = picked; applyLocalDir(); })}>{tr("settings.browse")}</button>
        </div>
      </div>
    {:else}
      <code class="storage-path" id="storagePath" title={destination || tr("settings.defaultHubCache")}>{destination || tr("settings.defaultHubCache")}</code>
    {/if}
    <details class="storage-advanced" id="typeFoldersGroup">
      <summary title={tr("settings.typeFoldersHint")}>{tr("settings.typeFolders")}</summary>
      <p class="form-hint">{tr("settings.typeFoldersHint")}</p>
      <div class="storage-routes">
        {#each ROUTE_LABELS as route (route.key)}
          <div class="transport-field">
            <label class="transport-label" for={"routeJobs" + route.key.replace(/[^a-z]/gi, "")}>{route.label}</label>
            <div class="input-with-browse">
              <input type="text" id={"routeJobs" + route.key.replace(/[^a-z]/gi, "")} class="path-input" spellcheck="false" bind:value={storage.routes[route.key]} onchange={applyRoutes} />
              <button type="button" class="btn btn-secondary btn-sm" onclick={() => openPicker(storage.routes[route.key], (picked) => { storage.routes[route.key] = picked; applyRoutes(); })}>{tr("settings.browse")}</button>
            </div>
          </div>
        {/each}
        <div class="transport-field storage-preset-field">
          <label class="transport-label" for="routePresetRootJobs" title={tr("settings.presetRootHint")}>{tr("settings.presetRoot")}</label>
          <div class="input-with-browse">
            <input type="text" id="routePresetRootJobs" class="path-input" placeholder="/mnt/nvme/models" spellcheck="false" bind:value={storage.presetRoot} />
            <button type="button" class="btn btn-secondary btn-sm" onclick={fillRoutePreset}>{tr("settings.fillFolders")}</button>
            <button type="button" class="btn btn-secondary btn-sm" onclick={() => openPicker(storage.presetRoot, (picked) => (storage.presetRoot = picked))}>{tr("settings.browse")}</button>
          </div>
        </div>
      </div>
    </details>
  </div>
</div>

<div class="jobs-list">
  <JobList />
</div>

<Modal title={tr("settings.folderPicker")} open={picker.open} onclose={() => (picker.open = false)}>
  {#if picker.open}
    <FolderBrowser path={picker.path} onpick={(picked) => { picker.apply?.(picked); picker.open = false; }} />
  {/if}
</Modal>