// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

import { test, expect } from "vitest";
import { mount, tick, unmount } from "svelte";
import Cache from "../src/pages/Cache.svelte";
import Jobs from "../src/pages/Jobs.svelte";
import Mirror from "../src/pages/Mirror.svelte";
import { ROUTE_LABELS } from "../src/routes.js";
import { ui, loadSettings } from "../src/store.svelte.js";

// Every path field has to be pickable, not just typeable, and the picker has to
// work on both platforms: Windows needs a way off the drive it opened on (a volume
// root has no parent and one drive is invisible from inside another), and macOS
// needs "~" and deep paths to be reachable, which the jump field and the server
// expansion cover.
const roots = [
  { name: "C:", path: "C:\\" },
  { name: "D:", path: "D:\\" }
];
const settingsPayload = {
  storageMode: "cache",
  configuredCacheDir: "C:\\cache",
  effectiveHubDir: "C:\\cache\\hub",
  localDir: "",
  localScanDirs: [],
  downloadRoutes: { "llm/gguf": "C:\\cache\\llm" },
  maxSpeed: "",
  connections: 8,
  maxActive: 3,
  retries: 4,
  verify: "size"
};
const listings = {
  "C:\\cache": {
    path: "C:\\cache",
    parent: "C:\\",
    roots,
    entries: [{ name: "Alpha", path: "C:\\cache\\Alpha" }, { name: "beta", path: "C:\\cache\\beta", is_symlink: true }],
    limit: 500
  },
  "C:\\cache\\Alpha": { path: "C:\\cache\\Alpha", parent: "C:\\cache", roots, entries: [], limit: 500 },
  "C:\\": { path: "C:\\", parent: "", roots, entries: [{ name: "cache", path: "C:\\cache" }], limit: 500 },
  "C:\\cache\\llm": { path: "C:\\cache\\llm", parent: "C:\\cache", roots, entries: [], limit: 500 },
  "C:\\cache\\hub": { path: "C:\\cache\\hub", parent: "C:\\cache", roots, entries: [{ name: "models", path: "C:\\cache\\hub\\models" }], limit: 500 },
  "C:\\cache\\hub\\models": { path: "C:\\cache\\hub\\models", parent: "C:\\cache\\hub", roots, entries: [], limit: 500 },
  "D:\\": { path: "D:\\", parent: "", roots, entries: [{ name: "Models", path: "D:\\Models" }], limit: 500 },
  "E:\\picked": { path: "E:\\picked", parent: "E:\\", roots, entries: [], limit: 500 },
  // A server with a desktop session advertises the operating system dialog, which is
  // the only way to reach a folder the listing does not show.
  "C:\\native": { path: "C:\\native", parent: "C:\\", roots, entries: [{ name: "Deep", path: "C:\\native\\Deep" }], limit: 500, native_pick: true }
};
let settingsState = { ...settingsPayload };
const browseCalls = [];
const pickCalls = [];
let pickResult = { path: "D:\\Models", available: true };

global.fetch = async (url, options = {}) => {
  const target = String(url);
  if (target.startsWith("/api/settings")) {
    if (String(options.method || "").toUpperCase() === "POST") {
      const body = options.body ? JSON.parse(options.body) : {};
      if (Object.prototype.hasOwnProperty.call(body, "cacheDir")) settingsState.configuredCacheDir = body.cacheDir;
    }
    return { ok: true, json: async () => settingsState };
  }
  if (target.startsWith("/api/browse/pick")) {
    const start = decodeURIComponent(target.split("?path=")[1] || "");
    pickCalls.push({ start, method: String(options.method || "") });
    return { ok: true, status: 200, json: async () => pickResult };
  }
  if (target.startsWith("/api/browse")) {
    const path = decodeURIComponent(target.split("?path=")[1] || "");
    browseCalls.push(path);
    const listing = listings[path];
    if (!listing) return { ok: false, status: 404, json: async () => ({ error_detail: { code: "not_found", message: "Cannot list folder" } }) };
    return { ok: true, status: 200, json: async () => listing };
  }
  return { ok: true, status: 200, json: async () => ({ jobs: [], repos: [], targets: [], entries: [], stats: null }) };
};

async function flush() {
  for (let i = 0; i < 6; i += 1) {
    await tick();
    await new Promise((resolve) => setTimeout(resolve, 0));
  }
  await tick();
}

function resetState() {
  document.body.innerHTML = "";
  browseCalls.length = 0;
  pickCalls.length = 0;
  pickResult = { path: "D:\\Models", available: true };
  ui.lang = "en";
  ui.page = "cache";
  ui.jobs.clear();
  ui.jobActions.clear();
  ui.cache.repos = [];
  ui.cache.stats = null;
  ui.cache.busy = false;
  ui.cache.details = null;
  ui.cache.selection = null;
  ui.cache.outcome = null;
  ui.mirror.targets = [];
  ui.mirror.diff = null;
  settingsState = { ...settingsPayload };
  ui.settings = { ...settingsPayload };
}

function commit(el, value) {
  el.value = value;
  el.dispatchEvent(new window.Event("input", { bubbles: true }));
}

test("the cache page path field opens the folder picker and applies the picked folder", async () => {
  resetState();
  const component = mount(Cache, { target: document.body });
  await flush();

  const browseButton = document.querySelector("#cacheDirGroup .input-with-browse button");
  expect(browseButton).not.toBe(null);
  browseButton.click();
  await flush();

  const items = [...document.querySelectorAll(".folder-item")];
  expect(items.map((item) => item.textContent.trim())).toEqual(["Alpha", "beta symlink"]);

  items[0].click();
  await flush();
  expect(document.querySelector(".folder-picker-path").textContent).toBe("C:\\cache\\Alpha");

  document.querySelector(".folder-picker-actions .btn").click();
  await tick();

  expect(document.querySelector("#cacheDirInput").value).toBe("C:\\cache\\Alpha");
  expect(document.querySelector(".folder-picker")).toBe(null);

  unmount(component);
});

// The Hub path is derived by the server, so it stays read-only, but the folder it
// sits in is still reachable: the pick writes the root that path is derived from.
test("the read-only Hub path can be browsed and the pick lands on the root it derives from", async () => {
  resetState();
  const component = mount(Cache, { target: document.body });
  await flush();

  const browseButton = document.querySelector("#cacheHubDirGroup .input-with-browse button");
  expect(browseButton).not.toBe(null);
  browseButton.click();
  await flush();

  // The picker opens where the downloads actually are, not at an unrelated folder.
  expect(browseCalls[0]).toBe("C:\\cache\\hub");

  [...document.querySelectorAll(".folder-item")][0].click();
  await flush();
  document.querySelector(".folder-picker-actions .btn").click();
  await tick();

  expect(document.querySelector("#cacheDirInput").value).toBe("C:\\cache\\hub\\models");
  expect(settingsState.configuredCacheDir).toBe("C:\\cache\\hub\\models");
  expect(document.querySelector("#cacheHubDir").value).toBe("C:\\cache\\hub");

  unmount(component);
});

test("the picker offers the drive roots, so a Windows listing is not stuck on one drive", async () => {
  resetState();
  const component = mount(Cache, { target: document.body });
  await flush();

  document.querySelector("#cacheDirGroup .input-with-browse button").click();
  await flush();

  const driveButtons = [...document.querySelectorAll(".folder-root-btn")];
  expect(driveButtons.map((button) => button.textContent.trim())).toEqual(["C:", "D:"]);

  driveButtons[1].click();
  await flush();
  expect(document.querySelector(".folder-picker-path").textContent).toBe("D:\\");
  expect([...document.querySelectorAll(".folder-item")].map((item) => item.textContent.trim())).toEqual(["Models"]);

  document.querySelector(".folder-picker-actions .btn").click();
  await tick();
  expect(document.querySelector("#cacheDirInput").value).toBe("D:\\");

  unmount(component);
});

test("the picker jumps to a typed path, which is how ~ and a deep folder are reached", async () => {
  resetState();
  const component = mount(Cache, { target: document.body });
  await flush();

  document.querySelector("#cacheDirGroup .input-with-browse button").click();
  await flush();

  const jump = document.querySelector("#folderJumpPath");
  expect(jump).not.toBe(null);
  commit(jump, "E:\\picked");
  jump.dispatchEvent(new window.KeyboardEvent("keydown", { key: "Enter", bubbles: true }));
  await flush();

  expect(browseCalls).toContain("E:\\picked");
  expect(document.querySelector(".folder-picker-path").textContent).toBe("E:\\picked");

  unmount(component);
});

test("every download folder on the jobs page can be picked, not only typed", async () => {
  resetState();
  ui.page = "jobs";
  const component = mount(Jobs, { target: document.body });
  await flush();

  const routeFields = [...document.querySelectorAll("#typeFoldersGroup .input-with-browse")];
  expect(routeFields.length).toBeGreaterThanOrEqual(6);
  for (const field of routeFields) expect(field.querySelector("button")).not.toBe(null);

  const gguf = document.querySelector("#routeJobsllmgguf");
  expect(gguf.value).toBe("C:\\cache\\llm");
  gguf.closest(".input-with-browse").querySelector("button").click();
  await flush();
  expect(document.querySelector(".folder-picker")).not.toBe(null);

  // The stored route folder is listed, so the picker opens inside it; the drive row
  // is how the user gets back out to pick a different one.
  expect(document.querySelector(".folder-picker-path").textContent).toBe("C:\\cache\\llm");
  document.querySelectorAll(".folder-root-btn")[0].click();
  await flush();
  [...document.querySelectorAll(".folder-item")].find((item) => item.textContent.trim().startsWith("cache")).click();
  await flush();
  expect(document.querySelector(".folder-picker-path").textContent).toBe("C:\\cache");

  document.querySelector(".folder-picker-actions .btn").click();
  await tick();
  expect(document.querySelector("#routeJobsllmgguf").value).toBe("C:\\cache");

  unmount(component);
});


// The system dialog is opened by the server, which owns the filesystem, and hands
// back a real absolute path. Two answers matter: the folder that was picked, and a
// cancellation that must not look like a picked empty path.
test("the system folder dialog fills the field with the folder the OS returned", async () => {
  resetState();
  const component = mount(Cache, { target: document.body });
  await flush();

  document.querySelector("#cacheDirGroup .input-with-browse button").click();
  await flush();

  // A listing from a server with no desktop session offers no dialog button.
  expect(document.querySelector(".folder-picker-actions .btn-primary").textContent.trim()).toBe("Choose this folder");

  const jump = document.querySelector("#folderJumpPath");
  commit(jump, "C:\\native");
  jump.dispatchEvent(new window.KeyboardEvent("keydown", { key: "Enter", bubbles: true }));
  await flush();

  const nativeButton = document.querySelector(".folder-picker-actions .btn-primary");
  expect(nativeButton.textContent.trim()).toBe("Choose in system dialog");
  nativeButton.click();
  await flush();

  expect(pickCalls).toEqual([{ start: "C:\\native", method: "POST" }]);
  expect(document.querySelector("#cacheDirInput").value).toBe("D:\\Models");
  expect(document.querySelector(".folder-picker")).toBe(null);

  unmount(component);
});

test("a cancelled system dialog leaves the field alone and says so", async () => {
  resetState();
  pickResult = { cancelled: true, available: true };
  const component = mount(Cache, { target: document.body });
  await flush();

  document.querySelector("#cacheDirGroup .input-with-browse button").click();
  await flush();
  const jump = document.querySelector("#folderJumpPath");
  commit(jump, "C:\\native");
  jump.dispatchEvent(new window.KeyboardEvent("keydown", { key: "Enter", bubbles: true }));
  await flush();

  const before = document.querySelector("#cacheDirInput").value;
  document.querySelector(".folder-picker-actions .btn-primary").click();
  await flush();

  expect(document.querySelector("#cacheDirInput").value).toBe(before);
  expect(document.querySelector(".folder-picker")).not.toBe(null);
  expect(document.querySelector(".folder-picker").textContent).toContain("cancelled");

  unmount(component);
});

test("the mirror target path can be picked too", async () => {
  resetState();
  ui.page = "mirror";
  const component = mount(Mirror, { target: document.body });
  await flush();

  // The path field lives in the "add target" dialog, so open it first.
  document.querySelector(".page-actions .btn-primary").click();
  await flush();
  const pathField = document.querySelector("#mirror-path");
  commit(pathField, "C:\\cache");
  pathField.closest(".input-with-browse").querySelector("button").click();
  await flush();
  expect(document.querySelector(".folder-picker")).not.toBe(null);

  [...document.querySelectorAll(".folder-item")][0].click();
  await flush();
  document.querySelector(".folder-picker-actions .btn").click();
  await tick();
  expect(document.querySelector("#mirror-path").value).toBe("C:\\cache\\Alpha");

  unmount(component);
});

// A style pass must not quietly drop a browse affordance: every folder field on the
// pages that write to disk keeps a button next to it, including the server-derived
// Hub path, which stays read-only output but can still be browsed from.
test("every folder field on the pages that write to disk has a browse button", async () => {
  const pages = [
    { name: "cache", page: Cache, fields: 3 },
    { name: "jobs", page: Jobs, fields: ROUTE_LABELS.length + 1 },
    { name: "mirror", page: Mirror, fields: 1 }
  ];

  for (const entry of pages) {
    resetState();
    ui.page = entry.name;
    const component = mount(entry.page, { target: document.body });
    await flush();
    if (entry.name === "mirror") {
      // The target path lives in the "add target" dialog.
      document.querySelector(".page-actions .btn-primary").click();
      await flush();
    }

    const fields = [...document.querySelectorAll(".input-with-browse")];
    expect(fields.length).toBe(entry.fields);
    for (const field of fields) {
      expect(field.querySelector("input.path-input, textarea.path-input")).not.toBe(null);
      expect(field.querySelector("button")).not.toBe(null);
    }

    unmount(component);
  }

  resetState();
  ui.page = "cache";
  const component = mount(Cache, { target: document.body });
  await flush();
  const hubDir = document.querySelector("#cacheHubDir");
  expect(hubDir.readOnly).toBe(true);
  expect(hubDir.closest(".input-with-browse")).not.toBe(null);

  // The three roots are one aligned grid: same row class, same label, same button,
  // and none of them hidden behind a disclosure.
  const rows = [...document.querySelectorAll("#cacheLocations .cache-location-row")];
  expect(rows.length).toBe(3);
  expect(document.querySelector("#cacheLocations details")).toBe(null);
  for (const row of rows) {
    expect(row.querySelector(".cache-location-label")).not.toBe(null);
    expect(row.querySelector(".input-with-browse button")).not.toBe(null);
  }
  unmount(component);
});
