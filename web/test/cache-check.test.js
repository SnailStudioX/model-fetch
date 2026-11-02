// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

import { test, expect, afterEach } from "vitest";
import { mount, tick, unmount } from "svelte";
import Cache from "../src/pages/Cache.svelte";
import { ui } from "../src/store.svelte.js";

// A cache check is a task: the page starts it, follows it while it runs, and then
// shows what it found. The two passes mean different things, so the panel says
// which one ran - "checked" without that would read as more assurance than the
// user actually bought.

const cachedEntry = {
  repo: "owner/name",
  owner: "owner",
  name: "name",
  type: "model",
  size: 0,
  sizeHuman: "0 B",
  fileCount: 3,
  downloaded: "2026-01-01",
  downloadStatus: "complete",
  source: "HF cache",
  commit: "",
  tags: []
};

const realFetch = globalThis.fetch;
let component = null;
let calls = [];

function stubFetch({ started = null, task = null, result = null } = {}) {
  calls = [];
  globalThis.fetch = async (url, options = {}) => {
    const path = String(url);
    const method = options.method || "GET";
    calls.push(method + " " + path);
    if (method === "GET" && path === "/api/cache-check") {
      const finished = task && result ? { ...task, result } : task;
      return { ok: true, status: 200, json: async () => ({ task: finished ?? null }) };
    }
    if (method === "POST" && path === "/api/cache-check") {
      return { ok: true, status: started ? 202 : 200, json: async () => ({ task: started }) };
    }
    return {
      ok: true,
      status: 200,
      json: async () => ({
        repos: [cachedEntry],
        stats: { totalModels: 1, totalDatasets: 0, totalSize: 0, totalSizeHuman: "0 B", totalFiles: 0 },
        cacheDir: "C:\\cache",
        effectiveHubDir: "C:\\cache\\hub"
      })
    };
  };
}

async function flush() {
  await tick();
  await new Promise((resolve) => setTimeout(resolve, 0));
  await tick();
}

async function openPage(options) {
  stubFetch(options);
  document.body.innerHTML = "";
  ui.lang = "en";
  ui.toasts.length = 0;
  ui.cache.busy = false;
  ui.cache.view = "list";
  ui.cache.filter = "all";
  ui.cache.search = "";
  ui.cache.check = null;
  ui.cache.repos = [cachedEntry];
  ui.cache.details = null;
  ui.cache.outcome = null;
  component = mount(Cache, { target: document.body });
  await flush();
}

afterEach(() => {
  globalThis.fetch = realFetch;
  if (component) unmount(component);
  component = null;
  document.body.innerHTML = "";
  ui.toasts.length = 0;
  ui.cache.check = null;
});

const runningCheck = {
  id: "cachecheck-abc",
  state: "running",
  phase: "checking",
  currentRepo: "owner/two",
  totalRepos: 3,
  doneRepos: 1,
  scannedBytes: 0,
  problems: 0,
  verifyBytes: false
};

const problemReport = {
  success: false,
  verifyBytes: false,
  reposChecked: 3,
  reposClean: 2,
  reposProblem: 1,
  reposUnreadable: 0,
  blobs: 12,
  bytes: 4096,
  bytesHuman: "4.0 KB",
  stagingFiles: 1,
  stagingBytes: 512,
  problems: 2,
  repos: [
    {
      repo: "owner/broken",
      type: "model",
      blobs: 3,
      bytes: 4096,
      problems: [
        { kind: "hash-mismatch", path: "blobs/aaaa", detail: "stored as aaaa, content hashes to bbbb", size: 2048 },
        { kind: "empty-blob", path: "blobs/cccc", size: 0 }
      ]
    }
  ],
  message: "3 repositories checked: 1 with problems, 0 could not be read"
};

test("starting a check keeps a progress panel for the running pass", async () => {
  await openPage({ started: runningCheck });

  document.querySelector("#cacheCheckBtn").click();
  await flush();

  const panel = document.querySelector("#cacheCheck");
  expect(panel).toBeTruthy();
  expect(panel.textContent).toContain("1/3");
  expect(panel.textContent).toContain("owner/two");
  expect(document.querySelector("#cacheCheckCancel")).toBeTruthy();
  expect(calls.some((entry) => entry === "POST /api/cache-check")).toBe(true);
  // A second pass would read the same files twice for no reason.
  expect(document.querySelector("#cacheCheckBtn").disabled).toBe(true);
});

test("opening the page reattaches to a check already running", async () => {
  await openPage({ task: runningCheck });

  const panel = document.querySelector("#cacheCheck");
  expect(panel).toBeTruthy();
  expect(panel.textContent).toContain("1/3");
  expect(calls.some((entry) => entry === "GET /api/cache-check")).toBe(true);
});

// The report is the point of the feature: which repositories are wrong, which file,
// and what was expected. A count alone would not tell the user what to do next.
test("the finished report names each affected repository and what is wrong", async () => {
  await openPage({ task: { ...runningCheck, state: "completed", doneRepos: 3 }, result: problemReport });

  const list = document.querySelector("#cacheCheckProblems");
  expect(list).toBeTruthy();
  expect(list.textContent).toContain("owner/broken");
  expect(list.textContent).toContain("Content does not match its name");
  expect(list.textContent).toContain("Empty file");
  expect(list.textContent).toContain("blobs/aaaa");
  expect(document.querySelector("#cacheCheck").textContent).toContain("Quick check");
  // Unfinished download files are reported, not counted as damage.
  expect(document.querySelector("#cacheCheck").textContent).toContain("unfinished download files");
});

// A report is bounded so one failing disk cannot produce a megabyte of findings.
// The counts stay complete, so the page has to say what it left out — otherwise
// the twenty findings shown read as the whole problem.
test("a bounded report says what it left out", async () => {
  await openPage({
    task: { ...runningCheck, state: "completed", doneRepos: 3 },
    result: {
      ...problemReport,
      problems: 25,
      reposProblem: 8,
      reposOmitted: 7,
      repos: [{ ...problemReport.repos[0], problemsOmitted: 23 }]
    }
  });

  const moreFindings = document.querySelector("#cacheCheckMoreFindings");
  expect(moreFindings).toBeTruthy();
  expect(moreFindings.textContent).toContain("23");
  expect(document.querySelector("#cacheCheck").textContent).toContain("7 more repositories");
});

test("a quick report offers the byte pass, a byte report does not", async () => {
  await openPage({ task: { ...runningCheck, state: "completed", doneRepos: 3 }, result: problemReport });
  expect(document.querySelector("#cacheCheckBytes")).toBeTruthy();

  document.querySelector("#cacheCheckBytes").click();
  await flush();
  const posted = calls.filter((entry) => entry === "POST /api/cache-check");
  expect(posted.length).toBe(1);

  await openPage({
    task: { ...runningCheck, state: "completed", doneRepos: 3, verifyBytes: true },
    result: { ...problemReport, verifyBytes: true, scannedBytes: 4096 }
  });
  expect(document.querySelector("#cacheCheckBytes")).toBeNull();
  expect(document.querySelector("#cacheCheck").textContent).toContain("Byte check");
});

test("a clean library is reported as clean", async () => {
  await openPage({
    task: { ...runningCheck, state: "completed", doneRepos: 3 },
    result: {
      success: true,
      verifyBytes: true,
      reposChecked: 3,
      reposClean: 3,
      reposProblem: 0,
      reposUnreadable: 0,
      blobs: 9,
      bytes: 1024,
      bytesHuman: "1.0 KB",
      scannedBytes: 1024,
      message: "3 repositories checked, all intact (1.0 KB re-read)"
    }
  });

  expect(document.querySelector("#cacheCheckClean")).toBeTruthy();
  expect(document.querySelector("#cacheCheckProblems")).toBeNull();
  const toast = ui.toasts[ui.toasts.length - 1];
  expect(toast.kind).toBe("success");
  expect(toast.message).toContain("Checked 3 repositories");
});

// A stopped pass has only seen part of the library; that has to be said, or the
// absence of findings reads as a clean bill of health.
test("a cancelled pass says the library was only partly seen", async () => {
  await openPage({
    task: { ...runningCheck, state: "cancelled", doneRepos: 1 },
    result: { ...problemReport, repos: [], reposChecked: 1, reposClean: 1, reposProblem: 0, success: true, message: "cancelled after 1 of 3 repositories" }
  });

  const toast = ui.toasts[ui.toasts.length - 1];
  expect(toast.kind).toBe("warning");
  expect(toast.message).toContain("only part of the library was looked at");
});
