// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

import { test, expect, beforeEach, afterEach } from "vitest";
import { mount, tick, unmount } from "svelte";
import JobsPage from "../src/pages/Jobs.svelte";
import CachePage from "../src/pages/Cache.svelte";
import SettingsPage from "../src/pages/Settings.svelte";
import { ui } from "../src/store.svelte.js";

// The Settings page used to hold one storage card for every path in the app. It
// is split by who needs the answer: the Jobs page carries the download target
// (layout, local folder, per-type folders) because that is decided while jobs are
// started, and the Cache page carries the roots it scans because that is what
// the list below it is made of. Both post partial bodies; Settings no longer
// sends these fields at all, so saving it cannot roll them back.
let posts = [];
let stored = {};
let realFetch = null;
let component = null;
let node = null;

function fakeFetch() {
  globalThis.fetch = async (url, options = {}) => {
    const method = options.method || "GET";
    if (method === "POST") {
      const body = options.body ? JSON.parse(options.body) : null;
      posts.push({ url: String(url), body });
      stored = { ...stored, ...body };
      stored.storageMode = String(stored.localDir || "") ? "local" : "cache";
    }
    if (String(url).startsWith("/api/cache")) {
      return { ok: true, status: 200, json: async () => ({ repos: [], stats: { totalModels: 0, totalDatasets: 0, totalSize: 0, totalSizeHuman: "0 B", totalFiles: 0 }, cacheDir: stored.configuredCacheDir, effectiveHubDir: stored.effectiveHubDir }) };
    }
    return { ok: true, status: 200, json: async () => stored };
  };
}

async function settle() {
  await tick();
  await new Promise((resolve) => setTimeout(resolve, 0));
  await tick();
}

function commit(el, value) {
  el.value = value;
  el.dispatchEvent(new window.Event("input", { bubbles: true }));
  el.dispatchEvent(new window.Event("change", { bubbles: true }));
}

function selectOption(el, value) {
  el.value = value;
  el.dispatchEvent(new window.Event("change", { bubbles: true }));
}

function mountPage(componentClass) {
  node = document.createElement("div");
  document.body.appendChild(node);
  component = mount(componentClass, { target: node, props: {} });
}

beforeEach(() => {
  realFetch = globalThis.fetch;
  posts = [];
  stored = {
    storageMode: "cache",
    cacheDir: "C:\\cache",
    configuredCacheDir: "C:\\cache",
    effectiveHubDir: "C:\\cache\\hub",
    hubDirSource: "cacheDir",
    localDir: "D:\\Models",
    localScanDirs: ["D:\\Models"],
    downloadRoutes: { "llm/gguf": "C:\\models\\LLM\\GGUF" },
    maxSpeed: "",
    connections: 8,
    maxActive: 3,
    retries: 4,
    verify: "size"
  };
  ui.lang = "en";
  ui.jobs.clear();
  ui.jobActions.clear();
  ui.cache.repos = [];
  ui.cache.stats = null;
  ui.cache.busy = false;
  ui.cache.details = null;
  ui.cache.selection = null;
  ui.cache.outcome = null;
  ui.settings = { ...stored };
  fakeFetch();
});

afterEach(() => {
  globalThis.fetch = realFetch;
  if (component) unmount(component);
  if (node) node.remove();
  component = null;
  node = null;
  document.body.innerHTML = "";
});

test("the Jobs page carries the download target under the transport knobs", async () => {
  ui.page = "jobs";
  mountPage(JobsPage);
  await settle();

  const row = node.querySelector(".storage-row");
  expect(row).toBeTruthy();
  expect(row.querySelector("#downloadLayoutJobs").value).toBe("cache");
  // The readout names the directory a new download would actually write into.
  expect(node.querySelector("#storagePath").textContent).toBe("C:\\cache\\hub");
});

test("switching the layout posts only the field that decides it", async () => {
  ui.page = "jobs";
  mountPage(JobsPage);
  await settle();

  selectOption(node.querySelector("#downloadLayoutJobs"), "local");
  await settle();
  expect(posts[posts.length - 1].body).toEqual({ localDir: "D:\\Models" });
  expect(node.querySelector("#localDirJobs").value).toBe("D:\\Models");

  selectOption(node.querySelector("#localDirJobs").closest(".storage-row").querySelector("#downloadLayoutJobs"), "cache");
  await settle();
  expect(posts[posts.length - 1].body).toEqual({ localDir: "" });
  expect(node.querySelector("#storagePath")).toBeTruthy();
});

test("switching to the local layout without a folder opens the field instead of writing an empty target", async () => {
  stored.localDir = "";
  stored.storageMode = "cache";
  ui.settings = { ...stored };
  ui.page = "jobs";
  mountPage(JobsPage);
  await settle();

  selectOption(node.querySelector("#downloadLayoutJobs"), "local");
  await settle();
  expect(posts.filter((entry) => entry.url === "/api/settings")).toEqual([]);
  expect(node.querySelector("#localDirJobs")).toBeTruthy();

  commit(node.querySelector("#localDirJobs"), "E:\\Models");
  await settle();
  expect(posts[posts.length - 1].body).toEqual({ localDir: "E:\\Models" });
});

test("a type folder posts the loaded route map with only that entry changed", async () => {
  ui.page = "jobs";
  mountPage(JobsPage);
  await settle();

  const audio = node.querySelector("#routeJobsaudio");
  expect(audio.value).toBe("");
  commit(audio, "C:\\models\\Audio");
  await settle();
  expect(posts[posts.length - 1].body).toEqual({ downloadRoutes: { "llm/gguf": "C:\\models\\LLM\\GGUF", audio: "C:\\models\\Audio" } });

  commit(node.querySelector("#routeJobsllmgguf"), "");
  await settle();
  expect(posts[posts.length - 1].body).toEqual({ downloadRoutes: { audio: "C:\\models\\Audio" } });
});

test("the Cache page shows the roots it scans and saves them where they are read", async () => {
  ui.page = "cache";
  mountPage(CachePage);
  await settle();

  const strip = node.querySelector("#cacheLocations");
  expect(strip).toBeTruthy();
  const hubDir = strip.querySelector("#cacheHubDir");
  expect(hubDir.readOnly).toBe(true);
  expect(hubDir.value).toBe("C:\\cache\\hub");
  expect(strip.querySelector("#cacheDirInput").value).toBe("C:\\cache");
  expect(strip.querySelector("#localScanDirs").value).toBe("D:\\Models");

  commit(strip.querySelector("#cacheDirInput"), "E:\\hf-cache");
  await settle();
  expect(posts[posts.length - 1].body).toEqual({ cacheDir: "E:\\hf-cache" });

  commit(strip.querySelector("#localScanDirs"), "D:\\Models\nE\\Extra");
  await settle();
  expect(posts[posts.length - 1].body).toEqual({ localScanDirs: ["D:\\Models", "E\\Extra"] });
});

test("the Cache page reads its listing on entry, not only after a manual refresh", async () => {
  ui.page = "cache";
  mountPage(CachePage);
  await settle();
  expect(node.querySelector("#cacheList")).toBeTruthy();
  expect(node.querySelector(".empty-state")).toBeTruthy();
});

test("the Settings page no longer carries the storage card", async () => {
  ui.page = "settings";
  mountPage(SettingsPage);
  await settle();

  for (const id of ["downloadLayout", "cacheDirInput", "localDirInput", "localScanDirs", "routePresetRoot", "effectiveHubDirInput"]) {
    expect(node.querySelector(`#${id}`)).toBe(null);
  }
  const headings = [...node.querySelectorAll(".settings-card h3")].map((el) => el.textContent.trim());
  expect(headings).not.toContain("Storage");
});