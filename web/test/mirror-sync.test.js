// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

import { test, expect, afterEach } from "vitest";
import { mount, tick, unmount } from "svelte";
import Mirror from "../src/pages/Mirror.svelte";
import { connect, ui } from "../src/store.svelte.js";

// A push or pull that writes is a task on the server: the request answers with
// the task, progress arrives over the socket, and the page shows where the copy
// is and lets the user stop it. A result that carries errors is never announced
// as a success - the counts can read "nothing to do" while the server could not
// read the destination at all.

const realFetch = globalThis.fetch;
let component = null;
let node = null;
let calls = [];

function stubFetch(started, task = null) {
  calls = [];
  globalThis.fetch = async (url, options = {}) => {
    const path = String(url);
    const method = options.method || "GET";
    calls.push(method + " " + path);
    if (path.includes("/api/mirror/targets")) {
      return { ok: true, status: 200, json: async () => ({ targets: [{ name: "remote", path: "D:\\mirror-target", exists: true }] }) };
    }
    if (path.includes("/api/cache")) {
      return {
        ok: true,
        status: 200,
        json: async () => ({
          repos: [],
          stats: { totalModels: 0, totalDatasets: 0, totalSize: 0, totalSizeHuman: "0 B", totalFiles: 0 },
          cacheDir: "C:\\cache",
          effectiveHubDir: "C:\\cache\\hub"
        })
      };
    }
    if (method === "GET" && path.includes("/api/mirror/task")) {
      return { ok: true, status: 200, json: async () => ({ task }) };
    }
    return { ok: true, status: started ? 202 : 200, json: async () => started };
  };
}

function pushButton() {
  const diff = node.querySelector("#mirrorDiffBtn");
  return [...node.querySelectorAll(".sync-actions-row button")].find((button) => button !== diff && !button.disabled);
}

async function openPage(started, task = null) {
  stubFetch(started, task);
  ui.lang = "en";
  ui.toasts.length = 0;
  ui.mirror.task = null;
  ui.mirror.targets = [];
  ui.mirror.target = "";
  ui.mirror.diff = null;
  node = document.createElement("div");
  document.body.appendChild(node);
  component = mount(Mirror, { target: node });
  // The page loads its targets on mount, and each request settles across
  // microtasks, so a timer is what actually leaves the controls usable.
  await new Promise((resolve) => setTimeout(resolve, 0));
  await tick();
  ui.mirror.target = "remote";
  await tick();
}

afterEach(() => {
  globalThis.fetch = realFetch;
  if (component) unmount(component);
  if (node) node.remove();
  component = null;
  node = null;
  ui.toasts.length = 0;
  ui.mirror.task = null;
  ui.mirror.targets = [];
  ui.mirror.target = "";
  ui.mirror.diff = null;
});

const runningTask = {
  id: "mirror-abc",
  mode: "push",
  target: "remote",
  state: "running",
  phase: "copying",
  currentRepo: "owner/two",
  totalRepos: 3,
  doneRepos: 1,
  copiedBytes: 1024
};

test("starting a write sync keeps a progress panel with the running task", async () => {
  await openPage({ task: runningTask });

  pushButton().click();
  await new Promise((resolve) => setTimeout(resolve, 0));
  await tick();

  const panel = node.querySelector("#mirrorTask");
  expect(panel).toBeTruthy();
  expect(panel.textContent).toContain("1/3");
  expect(panel.textContent).toContain("owner/two");
  expect(node.querySelector("#mirrorTaskCancel")).toBeTruthy();
  expect(calls.some((entry) => entry === "POST /api/mirror/push")).toBe(true);
  // A second sync would race the first over the same destination, so the
  // controls that start one are closed while a task is running.
  expect([...node.querySelectorAll(".sync-actions-row button")].every((button) => button.disabled)).toBe(true);
});

test("the cancel action addresses the running task by id", async () => {
  await openPage({ task: runningTask });
  pushButton().click();
  await new Promise((resolve) => setTimeout(resolve, 0));
  await tick();

  node.querySelector("#mirrorTaskCancel").click();
  await new Promise((resolve) => setTimeout(resolve, 0));
  await tick();

  expect(calls.some((entry) => entry === "POST /api/mirror/task/mirror-abc/cancel")).toBe(true);
});

// A sync outlives the request that started it, so opening the page while one is
// running has to show it: idle controls would read as "nothing is happening".
test("opening the page reattaches to a sync already running", async () => {
  await openPage(null, runningTask);

  const panel = node.querySelector("#mirrorTask");
  expect(panel).toBeTruthy();
  expect(panel.textContent).toContain("1/3");
  expect(calls.some((entry) => entry === "GET /api/mirror/task")).toBe(true);
});

// The outcome is announced once, from the task, wherever the task finished.
test("a finished sync carrying errors is not announced as a success", async () => {
  await openPage({ task: runningTask });
  pushButton().click();
  await new Promise((resolve) => setTimeout(resolve, 0));
  await tick();

  const socket = { close() {} };
  connect(() => {}, { factory: () => socket });
  socket.onmessage({
    data: JSON.stringify({
      type: "mirror_task",
      data: {
        ...runningTask,
        state: "completed",
        phase: "",
        doneRepos: 3,
        result: {
          success: false,
          copied: 0,
          deleted: 0,
          errors: ["read destination hub D:\\mirror-target\\hub: open D:\\mirror-target\\hub: The system cannot find the path specified."]
        }
      }
    })
  });
  await tick();

  expect(ui.toasts.length).toBe(1);
  expect(ui.toasts[0].kind).toBe("warning");
  expect(ui.toasts[0].message).toContain("cannot find the path specified");
});

test("a sync that moved data is announced as a success", async () => {
  await openPage({ task: runningTask });
  pushButton().click();
  await new Promise((resolve) => setTimeout(resolve, 0));
  await tick();

  ui.mirror.task = {
    ...runningTask,
    state: "completed",
    phase: "",
    doneRepos: 3,
    result: { success: true, copied: 2, copiedSize: 3000, copiedSizeHuman: "3 KB", deleted: 0 }
  };
  await tick();

  expect(ui.toasts.length).toBe(1);
  expect(ui.toasts[0].kind).toBe("success");
  expect(ui.toasts[0].message).toContain("3 KB");
});

test("a cancelled sync says so instead of reporting counts", async () => {
  await openPage({ task: runningTask });
  pushButton().click();
  await new Promise((resolve) => setTimeout(resolve, 0));
  await tick();

  // Stopped before any report came back: there are no counts to show, and
  // inventing "nothing changed" would read as a completed no-op.
  ui.mirror.task = { ...runningTask, state: "cancelled", phase: "" };
  await tick();

  expect(ui.toasts.length).toBe(1);
  expect(ui.toasts[0].kind).toBe("warning");
  expect(ui.toasts[0].message).toBe("The mirror sync was cancelled.");
});

// A cancelled force re-copy removes the repository the destination already had.
// The server names that in result.errors; a notice that only says "cancelled"
// leaves the user unable to tell that the destination now has less in it than
// before the sync started.
test("a cancelled sync reports what it moved and what it removed", async () => {
  await openPage({ task: runningTask });
  pushButton().click();
  await new Promise((resolve) => setTimeout(resolve, 0));
  await tick();

  ui.mirror.task = {
    ...runningTask,
    state: "cancelled",
    phase: "",
    doneRepos: 1,
    result: {
      success: false,
      copied: 1,
      copiedSizeHuman: "2 KB",
      deleted: 0,
      errors: ["interrupted re-copy of owner/one: the copy the destination already had was removed with the partial one"]
    }
  };
  await tick();

  expect(ui.toasts.length).toBe(1);
  expect(ui.toasts[0].kind).toBe("warning");
  expect(ui.toasts[0].message).toContain("cancelled");
  expect(ui.toasts[0].message).toContain("2 KB");
  expect(ui.toasts[0].message).toContain("owner/one");
});
