import { apiFetch, wsConnect } from "./api.js";
import { createCoalescer } from "./coalesce.js";
import { formatBytes } from "./format.js";
import { activeSource, hubSources, newHubBody, switchSourceBody } from "./source.js";
import { SvelteMap, SvelteSet } from "svelte/reactivity";

// Collections in $state are only reactive when they are the runtime's own
// SvelteMap/SvelteSet: a plain Map or Set is stored as-is, so writing to it never
// re-runs the render that read it. That is why job cards kept their old progress
// and why a pause or cancel looked like a dead button - the request succeeded and
// the state changed, but nothing repainted.
export const ui = $state({
  page: "models",
  jobs: new SvelteMap(),
  // Whether the last /api/jobs read failed. An empty map otherwise looks exactly
  // like "there are no downloads", which is what the page then claims.
  jobsLoadFailed: false,
  settings: {},
  // Endpoint each source was last seen with, for the duration of this page session.
  // A hub switch needs an endpoint, and the built-in default is not always what the
  // user runs that hub with.
  sourceEndpoints: {},
  settingsLoadFailed: false,
  hardware: null,
  diskFree: null,
  version: "",
  connected: false,
  toasts: [],
  lang: (typeof localStorage !== "undefined" && localStorage.getItem("modelfetch.lang")) || "zh",
  models: {
    query: "", results: [], nextUrl: "", page: 1, busy: false, warnings: [],
    sort: "recommend", type: "", gguf: true, datasets: false,
    repo: "", analysis: null, readmeHtml: "", readmeMarkdown: "", readmeBase: "", busy: false,
    dlStatus: {}
  },
  mirror: { targets: [], diff: null, busy: false, target: "", filter: "", search: "", diffFilter: "all", verify: false, force: false, deleteExtra: false, task: null },
  cache: { repos: [], stats: null, busy: false, loadFailed: false, warnings: [], search: "", filter: "all", sort: "name", view: "list", hubDir: "", hubDirIsHub: false, selection: null, busySelection: false, details: null, pendingDeletes: new SvelteSet(), selectedLocationId: "", selectedGroupId: "", outcome: null, confirm: null, check: null },
  history: { entries: [], busy: false, loadFailed: false },
  speed: { gauge: 0 },
  // Job ids with an action request in flight. The card disables its buttons
  // while the id is present, so a double click cannot fire two cancels and the
  // second one cannot answer a confusing "Job not found".
  jobActions: new SvelteSet()
});

// Progress events arrive per file, so a naive handler repaints on every message.
// Updates are coalesced into one flush per frame; the server's bytesPerSecond
// already excludes resumed/skipped bytes, so the UI only displays it.
const coalescer = createCoalescer(ui.jobs);

export function applyJobUpdate(job) {
  coalescer.apply(job);
}

// dropJob forgets a job completely: the rendered map and any update still
// queued for it. A card left behind after the server deleted the job is what
// the user sees as repeated "Job not found" toasts on the next click.
export function dropJob(id) {
  if (!id) return;
  coalescer.remove(id);
  ui.jobs.delete(id);
}

// Toasts are transient and the store owns their lifetime: an entry starts its
// exit animation after TOAST_TTL_MS (leaving) and is dropped once the animation
// has played. Ids come from a monotonic counter, because reusing an id after a
// shift() makes the keyed each-block keep the wrong node.
const TOAST_TTL_MS = 4000;
const TOAST_EXIT_MS = 300;
const TOAST_MAX = 5;
let toastSeq = 0;

export function toast(message, kind = "info") {
  const id = ++toastSeq;
  ui.toasts.push({ id, message, kind, leaving: false });
  // Over the cap the oldest live toast leaves too, instead of vanishing mid-animation.
  const live = ui.toasts.filter((item) => !item.leaving);
  if (live.length > TOAST_MAX) dismissToast(live[0].id);
  setTimeout(() => dismissToast(id), TOAST_TTL_MS);
  return id;
}

// dismissToast is idempotent: the cap and the TTL timer can both reach for the
// same toast, and the second call must not restart the exit animation.
export function dismissToast(id) {
  const index = ui.toasts.findIndex((item) => item.id === id);
  if (index < 0) return;
  const item = ui.toasts[index];
  if (item.leaving) return;
  item.leaving = true;
  setTimeout(() => {
    const at = ui.toasts.findIndex((entry) => entry.id === id);
    if (at >= 0) ui.toasts.splice(at, 1);
  }, TOAST_EXIT_MS);
}

export async function loadHealth() {
  const data = await apiFetch("/api/health");
  ui.version = data.version || "";
  return data;
}

// applyJobList replaces the rendered job set with a list from the server. The
// socket sends the same shape when it connects, so both paths must agree:
// merging alone keeps cards for jobs that no longer exist (dismissed here, or
// dismissed while the socket was down) and their next action 404s.
export function applyJobList(jobs) {
  if (!Array.isArray(jobs)) return;
  // Either path that delivers a full list - HTTP or the socket's snapshot on
  // (re)connect - is current data, so it clears the failed-read state.
  ui.jobsLoadFailed = false;
  const live = new Set();
  for (const job of jobs) {
    if (!job || !job.id) continue;
    live.add(job.id);
    ui.jobs.set(job.id, job);
  }
  for (const id of [...ui.jobs.keys()]) if (!live.has(id)) dropJob(id);
}

export async function loadJobs() {
  try {
    const jobs = await apiFetch("/api/jobs");
    applyJobList(jobs.jobs || []);
    return jobs;
  } catch (err) {
    ui.jobsLoadFailed = true;
    throw err;
  }
}

export async function loadSettings() {
  try {
    ui.settings = await apiFetch("/api/settings");
    ui.sourceEndpoints[activeSource(ui.settings)] = ui.settings.endpoint || "";
    ui.settingsLoadFailed = false;
  } catch (err) {
    ui.settingsLoadFailed = true;
    throw err;
  }
  return ui.settings;
}

export async function saveSettings(body) {
  const result = await apiFetch("/api/settings", { method: "POST", body: JSON.stringify(body) });
  await loadSettings();
  return result;
}

// switchSource applies a hub switch right away. Source and endpoint travel
// together (see source.js): the server rejects a source whose API shape does not
// fit the endpoint, and a rejected half-switch is what looks like a selector that
// does nothing. On rejection the settings are reloaded so every control shows the
// source the server actually has.
export async function switchSource(next, endpointValue) {
  try {
    return await saveSettings(switchSourceBody(ui.settings, next, endpointValue, ui.sourceEndpoints));
  } catch (err) {
    await loadSettings().catch(() => {});
    throw err;
  }
}

// useHubSource makes one saved hub access source the live one. Naming the entry is
// the whole request: the server keeps the live source, endpoint and token equal to
// the active entry, so a credential saved for one mirror travels with it instead of
// following the user to the next hub. A server that has no saved list still has to
// work, so it falls back to the older source switch, which sends the endpoint that
// belongs to the source.
export async function useHubSource(id) {
  if (Array.isArray(ui.settings?.hub_sources)) {
    try {
      return await saveSettings(newHubBody(id));
    } catch (err) {
      await loadSettings().catch(() => {});
      throw err;
    }
  }
  const entry = hubSources(ui.settings).find((item) => item.id === id);
  return await switchSource(entry ? entry.source : id);
}

export async function loadHardware(fresh = false) {
  try {
    ui.hardware = await apiFetch("/api/hardware" + (fresh ? "?fresh=1" : ""));
  } catch {
    ui.hardware = null;
  }
  return ui.hardware;
}

export async function loadDiskFree(path = "") {
  try {
    ui.diskFree = await apiFetch("/api/diskfree" + (path ? "?path=" + encodeURIComponent(path) : ""));
  } catch {
    ui.diskFree = null;
  }
  return ui.diskFree;
}

// Folder listing for the Settings folder picker. Read-only. Unlike the disk
// helpers this one keeps throwing, because the picker shows the failure.
export async function browseDirs(path = "") {
  return apiFetch("/api/browse" + (path ? "?path=" + encodeURIComponent(path) : ""));
}

// The browser cannot hand back an absolute local path, so the operating system
// dialog is opened by the server process that owns the filesystem. It blocks until
// the person picks a folder or dismisses the dialog, and throws when the server has
// no desktop to show one on.
export async function pickNativeFolder(path = "") {
  return apiFetch("/api/browse/pick" + (path ? "?path=" + encodeURIComponent(path) : ""), { method: "POST" });
}

// Disk guard mirrors the legacy behaviour: a disk-stat failure stays non-fatal,
// a hard floor blocks the download, and warn mode only notifies. Server-side
// validation remains authoritative.
export async function checkDownloadDiskSpace(body, warn = false) {
  let df;
  try {
    df = await apiFetch("/api/diskfree", { method: "POST", body: JSON.stringify(body) });
  } catch {
    return true;
  }
  if (df.free < 100 * 1024 * 1024) {
    toast("Not enough disk space: " + formatBytes(df.free) + " free", "error");
    return false;
  }
  if (warn && df.free < 2 * 1024 * 1024 * 1024) {
    toast("Low disk space warning: " + formatBytes(df.free) + " free on " + df.path, "warning");
  }
  return true;
}

// runJobAction addresses the job the way the server routes it. Cancelling is the
// job resource itself - DELETE /api/jobs/{id} - not a /cancel sub-resource, and
// that missing route is what made every Cancel button look dead.
export async function runJobAction(id, action) {
  if (!id || ui.jobActions.has(id)) return null;
  const path = action === "cancel"
    ? `/api/jobs/${encodeURIComponent(id)}`
    : `/api/jobs/${encodeURIComponent(id)}/${action}`;
  ui.jobActions.add(id);
  try {
    const result = await apiFetch(path, { method: action === "cancel" ? "DELETE" : "POST" });
    if (action === "dismiss") {
      // Dismissal deletes the job server-side, so there is no snapshot to apply.
      dropJob(id);
    } else if (result.job) {
      ui.jobs.set(id, result.job);
    }
    return result;
  } finally {
    ui.jobActions.delete(id);
  }
}

export async function startDownload(body) {
  const result = await apiFetch("/api/download", { method: "POST", body: JSON.stringify(body) });
  if (result.job) ui.jobs.set(result.job.id, result.job);
  return result;
}

export async function previewPlan(body) {
  return apiFetch("/api/plan", { method: "POST", body: JSON.stringify(body) });
}

// Per-quant download state is reconciled with the live job list: active jobs
// report running/queued/done, and stale running/queued entries (job gone,
// cancelled or failed) are ignored so the Download button reappears. 'done'
// survives because it is a fact about files already on disk.
// The result is a fresh map. Callers read this from a $derived, so it must not
// write store state: pruning the stored map here would make the derived mutate
// the state it reads (Svelte reports that as state_unsafe_mutation).
export function dlStatusFor(repo) {
  const stored = ui.models.dlStatus[repo] || {};
  const active = {};
  for (const job of ui.jobs.values()) {
    const visibleRepo = job.localRepo || job.repo;
    if (visibleRepo !== repo) continue;
    const key = (job.filters || []).join(",") || "__all__";
    if (job.status === "completed") active[key] = "done";
    else if (job.status === "running") active[key] = "running";
    else if (job.status === "queued") active[key] = "queued";
  }
  const map = {};
  for (const [key, status] of Object.entries(stored)) {
    if ((status === "running" || status === "queued") && !active[key]) continue;
    map[key] = status;
  }
  for (const [key, status] of Object.entries(active)) map[key] = status;
  return map;
}

export function setDlStatus(repo, filterValue, status) {
  const map = ui.models.dlStatus[repo] || {};
  map[filterValue] = status;
  ui.models.dlStatus[repo] = map;
}

export async function loadReadme(repo, { revision = "main", dataset = false } = {}) {
  const params = new URLSearchParams();
  if (revision) params.set("revision", revision);
  if (dataset) params.set("dataset", "true");
  const result = await apiFetch("/api/readme/" + repo.split("/").map(encodeURIComponent).join("/") + (params.toString() ? "?" + params.toString() : ""));
  ui.models.readmeHtml = result.html || "";
  ui.models.readmeMarkdown = result.markdown || "";
  ui.models.readmeBase = result.baseRawURL || "";
  return result;
}

export async function loadAnalyze(repo) {
  ui.models.busy = true;
  try {
    ui.models.repo = repo;
    ui.models.analysis = await apiFetch("/api/analyze/" + repo.split("/").map(encodeURIComponent).join("/"));
    return ui.models.analysis;
  } finally {
    ui.models.busy = false;
  }
}

// clearModelsDetail drops the detail panel as a unit. An analysis kept next to
// another repo's README renders half a panel from each of them.
export function clearModelsDetail() {
  ui.models.analysis = null;
  ui.models.repo = "";
  ui.models.readmeHtml = "";
  ui.models.readmeMarkdown = "";
  ui.models.readmeBase = "";
}

// openRepoDetail is the jump used by job cards. Naming the repo is not enough:
// the Models page renders its detail from ui.models.analysis, so the old click
// landed on the empty-state hint instead of the detail it promised. The analysis
// (and its README) has to be fetched for the click to arrive.
export async function openRepoDetail(repo, revision = "") {
  const name = String(repo || "").trim();
  if (!name) return null;
  ui.page = "models";
  if (ui.models.analysis && ui.models.analysis.repo === name) return ui.models.analysis;
  // A stale analysis would render the wrong detail while this request is in flight.
  clearModelsDetail();
  const data = await loadAnalyze(name);
  const rev = revision || data.branch || "main";
  if (rev !== "main") data.branch = rev;
  loadReadme(name, { revision: rev, dataset: Boolean(data.is_dataset) }).catch(() => {});
  return data;
}

export async function loadSearch(page = 1) {
  const query = ui.models.query.trim();
  ui.models.busy = true;
  if (page === 1) ui.models.warnings = [];
  try {
    // An empty query is the default listing rather than a skipped call: the
    // server answers a query-less search with the first page in the current sort,
    // so entering the page shows data instead of an empty hint.
    const params = new URLSearchParams({ sort: ui.models.sort, page: String(page) });
    if (query) params.set("q", query);
    // GGUF is a model tag, so it must not narrow a dataset search. The box is on
    // by default, and the DS checkbox replaces models with datasets, so applying
    // it there would return almost nothing.
    const filter = [ui.models.type, ui.models.gguf && !ui.models.datasets ? "gguf" : ""].filter(Boolean).join(",");
    if (filter) params.set("filter", filter);
    if (ui.models.datasets) params.set("datasets", "true");
    const result = await apiFetch("/api/search?" + params.toString());
    ui.models.results = page === 1 ? (result.results || []) : [...ui.models.results, ...(result.results || [])];
    ui.models.page = result.page || page;
    ui.models.nextUrl = result.next_url || "";
    // What the hub did differently from what was asked (an order it does not
    // offer, a page it capped). The rows look like a normal result set either
    // way, so this is the only place the difference can reach the user.
    ui.models.warnings = result.warnings || [];
    return result;
  } finally {
    ui.models.busy = false;
  }
}

export async function loadMirrorTargets() {
  const result = await apiFetch("/api/mirror/targets");
  ui.mirror.targets = result.targets || [];
  return result;
}

export async function addMirrorTarget(body) {
  return apiFetch("/api/mirror/targets", { method: "POST", body: JSON.stringify(body) });
}

export async function removeMirrorTarget(name) {
  return apiFetch(`/api/mirror/targets/${encodeURIComponent(name)}`, { method: "DELETE" });
}

export async function runMirrorDiff({ target, repoFilter = "" }) {
  ui.mirror.busy = true;
  try {
    const result = await apiFetch("/api/mirror/diff", { method: "POST", body: JSON.stringify({ target, repoFilter }) });
    ui.mirror.diff = result;
    return result;
  } finally {
    ui.mirror.busy = false;
  }
}

// A push or pull that writes is a task on the server, not a request to wait for:
// the response names the task, and its progress arrives over the socket or from
// loadMirrorTask. dryRun reads two trees and writes nothing, so it still answers
// with the result inline.
export async function runMirrorSync(mode, { target, repoFilter = "", dryRun = false, verify = false, deleteExtra = false, force = false }) {
  ui.mirror.busy = true;
  try {
    const result = await apiFetch(`/api/mirror/${mode}`, {
      method: "POST",
      body: JSON.stringify({ target, repoFilter, dryRun, verify, deleteExtra, force })
    });
    if (dryRun) return result;
    // The server's answer is a task; anything else means the request did not
    // start one, and pretending otherwise would leave the page with no progress
    // and no notice.
    if (!result?.task?.id) {
      throw new Error("Mirror sync returned no task");
    }
    ui.mirror.task = result.task;
    return ui.mirror.task;
  } finally {
    ui.mirror.busy = false;
  }
}

// loadMirrorTask reattaches to the running or most recent sync. A page opened
// while a sync is already going would otherwise show idle controls and no copy.
export async function loadMirrorTask() {
  const result = await apiFetch("/api/mirror/task");
  ui.mirror.task = result.task ?? null;
  return ui.mirror.task;
}

export async function cancelMirrorTask(id) {
  const result = await apiFetch(`/api/mirror/task/${encodeURIComponent(id)}/cancel`, { method: "POST" });
  if (result.task) ui.mirror.task = result.task;
  return result;
}

// A cache check is a task on the server, not a request to wait for: a byte pass
// re-reads the whole library, which is minutes of disk work on a NAS. The page
// follows the task the same way it follows a mirror sync.
export async function startCacheCheck({ verifyBytes = false, repoFilter = "" } = {}) {
  const result = await apiFetch("/api/cache-check", {
    method: "POST",
    body: JSON.stringify({ verifyBytes, repoFilter })
  });
  if (!result?.task?.id) {
    throw new Error("Cache check returned no task");
  }
  ui.cache.check = result.task;
  return ui.cache.check;
}

// loadCacheCheck reattaches to the running or most recent pass, so a page opened
// or reloaded mid-check still shows what was found.
export async function loadCacheCheck() {
  const result = await apiFetch("/api/cache-check");
  ui.cache.check = result.task ?? null;
  return ui.cache.check;
}

export async function cancelCacheCheck(id) {
  const result = await apiFetch(`/api/cache-check/${encodeURIComponent(id)}/cancel`, { method: "POST" });
  if (result.task) ui.cache.check = result.task;
  return result;
}

export async function loadCache() {
  ui.cache.busy = true;
  try {
    const result = await apiFetch("/api/cache");
    ui.cache.repos = result.repos || [];
    ui.cache.stats = result.stats || null;
    ui.cache.hubDir = result.effectiveHubDir || result.cacheDir || "";
    ui.cache.hubDirIsHub = Boolean(result.effectiveHubDir);
    // A listing that only partly succeeded says so; the page shows it rather
    // than letting an unreadable directory read as an empty cache.
    ui.cache.warnings = result.warnings || [];
    // The detail modal is a snapshot of one repository. Once that repository is gone
    // from the listing - deleted here, in another tab, or by a rebuild - the card
    // only offers buttons that can fail, so the modal closes with it.
    const details = ui.cache.details;
    if (details && !ui.cache.repos.some((item) => item.repo === details.repo && item.type === details.type)) {
      ui.cache.details = null;
      ui.cache.selection = null;
      ui.cache.outcome = null;
    }
    ui.cache.loadFailed = false;
    return result;
  } catch (err) {
    // The listing is what this page shows: a failed read must not be displayed
    // as "the cache is empty" (see Cache.svelte's empty state).
    ui.cache.loadFailed = true;
    ui.cache.warnings = [];
    throw err;
  } finally {
    ui.cache.busy = false;
  }
}

export async function loadCacheDetails(repo, type = "model") {
  const result = await apiFetch(`/api/cache/${repo.split("/").map(encodeURIComponent).join("/")}?type=${encodeURIComponent(type)}`);
  ui.cache.details = result;
  return result;
}

export async function loadCacheSelection(repo, locationId = "") {
  const params = new URLSearchParams({ repo, type: "model" });
  if (locationId) params.set("locationId", locationId);
  const result = await apiFetch("/api/cache-selection?" + params.toString());
  ui.cache.selection = result;
  return result;
}

export async function deleteCacheSelection(body) {
  return apiFetch("/api/cache-selection", { method: "DELETE", body: JSON.stringify(body) });
}

export async function rebuildCache() {
  return apiFetch("/api/cache/rebuild", { method: "POST", body: JSON.stringify({ clean: true }) });
}

export async function deleteCacheRepo(repo, type = "model") {
  return apiFetch(`/api/cache/${repo.split("/").map(encodeURIComponent).join("/")}?type=${encodeURIComponent(type)}`, { method: "DELETE" });
}

export async function loadHistory() {
  ui.history.busy = true;
  try {
    const result = await apiFetch("/api/history");
    ui.history.entries = result.entries || result.history || [];
    ui.history.loadFailed = false;
    return result;
  } catch (err) {
    // History.svelte renders an empty-state card when entries are empty; a
    // failed read has to say so instead of claiming there is no history.
    ui.history.loadFailed = true;
    throw err;
  } finally {
    ui.history.busy = false;
  }
}

// options is passed through to wsConnect so tests can inject a fake socket.
export function connect(onJobUpdate, options) {
  return wsConnect((message) => {
    if (message.type === "status") {
      ui.connected = Boolean(message.data?.connected);
      return;
    }
    // The first frame on every socket is the whole job list. Ignoring it left
    // each card frozen at whatever it last rendered, so after a reconnect the
    // page looked alive while no progress or speed ever moved again.
    if (message.type === "init") {
      applyJobList(message.data?.jobs);
      return;
    }
    if (message.type === "job_removed" && message.data?.id) {
      dropJob(message.data.id);
      return;
    }
    if (message.type === "job_update" && message.data?.id) {
      applyJobUpdate(message.data);
      onJobUpdate?.(message.data);
      return;
    }
    // A mirror sync reports the same way a job does: the server pushes the task
    // whenever it starts a repository, so the page shows progress it did not ask
    // for and a second tab sees a sync started elsewhere.
    if (message.type === "mirror_task" && message.data?.id) {
      ui.mirror.task = message.data;
      return;
    }
    // A cache check reports the same way, one frame per repository.
    if (message.type === "cache_check" && message.data?.id) {
      ui.cache.check = message.data;
      return;
    }
    if (message.type === "event") {
      toast(message.data?.message || "Event", message.data?.type === "error" ? "error" : "info");
    }
  }, options);
}
