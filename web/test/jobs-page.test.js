// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

import { test, expect, beforeEach, afterEach } from "vitest";
import { mount, tick, unmount } from "svelte";
import JobCard from "../src/components/JobCard.svelte";
import JobList from "../src/components/JobList.svelte";
import { ui, loadJobs, runJobAction, connect } from "../src/store.svelte.js";

// The reported bugs: some job buttons did nothing, and speed and progress stopped
// moving. Three separate causes, all client-side:
//   - the job store is a plain Map, and a plain Map inside $state is not reactive,
//     so a new snapshot never repainted the card it belonged to;
//   - cancel was sent to a route the server does not have;
//   - the socket's init frame, which carries the whole job list, was ignored.

let requests = [];
let realFetch;
let node = null;
let component = null;

function fakeFetch(body = () => ({ success: true })) {
  globalThis.fetch = async (url, options = {}) => {
    const method = options.method || "GET";
    requests.push(method + " " + String(url));
    return { ok: true, status: 200, json: async () => body(String(url), method) };
  };
}

beforeEach(() => {
  realFetch = globalThis.fetch;
  requests = [];
  ui.jobs.clear();
  ui.jobsLoadFailed = false;
  ui.jobActions.clear();
  ui.lang = "en";
});

afterEach(() => {
  globalThis.fetch = realFetch;
  if (component) unmount(component);
  if (node) node.remove();
  component = null;
  node = null;
});

function render(kind, props) {
  node = document.createElement("div");
  document.body.appendChild(node);
  component = mount(kind, { target: node, props });
  return node;
}

const read = (selector) => {
  const found = node.querySelector(selector);
  return found ? found.textContent.trim() : null;
};

test("a new job snapshot repaints the card that is already on screen", async () => {
  ui.jobs.set("job-1", { id: "job-1", repo: "owner/repo", status: "running", progress: { downloadedBytes: 100, totalBytes: 1000 } });
  render(JobList);
  await tick();
  expect(read('[data-role="pct"]')).toBe("10.0%");

  // This is the whole complaint: the socket delivered the frame, the store held it,
  // and the page still showed the old number.
  ui.jobs.set("job-1", { id: "job-1", repo: "owner/repo", status: "running", progress: { downloadedBytes: 600, totalBytes: 1000, bytesPerSecond: 1048576 } });
  await tick();

  expect(read('[data-role="pct"]')).toBe("60.0%");
  expect(read('[data-role="speed"]')).toBe("1 MB/s");
});

test("cancel addresses the job resource the server routes", async () => {
  fakeFetch();
  ui.jobs.set("job-1", { id: "job-1", status: "running" });

  await runJobAction("job-1", "cancel");

  // DELETE /api/jobs/{id}/cancel is not a route. The 404 it answered is what made
  // every Cancel button look dead.
  expect(requests).toEqual(["DELETE /api/jobs/job-1"]);
});

test("the other actions keep their sub-resource routes", async () => {
  fakeFetch();

  await runJobAction("job-1", "pause");
  await runJobAction("job-1", "retry");

  expect(requests).toEqual(["POST /api/jobs/job-1/pause", "POST /api/jobs/job-1/retry"]);
});

test("a second click while the first is in flight sends nothing extra", async () => {
  let release;
  const blocked = new Promise((resolve) => { release = resolve; });
  globalThis.fetch = async (url, options = {}) => {
    requests.push((options.method || "GET") + " " + String(url));
    await blocked;
    return { ok: true, status: 200, json: async () => ({ success: true }) };
  };

  const first = runJobAction("job-1", "cancel");
  await tick();
  expect(ui.jobActions.has("job-1")).toBe(true);
  await runJobAction("job-1", "cancel");
  expect(requests.length).toBe(1);

  release();
  await first;
  expect(ui.jobActions.has("job-1")).toBe(false);

  await runJobAction("job-1", "cancel");
  expect(requests.length).toBe(2);
});

test("the init frame of a fresh socket re-syncs the job list", () => {
  ui.jobs.set("gone", { id: "gone", status: "running" });
  const socket = { close() {} };

  connect(() => {}, { factory: () => socket });
  socket.onmessage({
    data: JSON.stringify({
      type: "init",
      data: { jobs: [{ id: "live", status: "running", progress: { downloadedBytes: 10, totalBytes: 100, bytesPerSecond: 5 } }] }
    })
  });

  // Ignoring that frame left every card frozen at its last frame after a
  // reconnect, which is what looked like progress refusing to update.
  expect([...ui.jobs.keys()]).toEqual(["live"]);
  expect(ui.jobs.get("live").progress.bytesPerSecond).toBe(5);
});

// An empty job list and an unreadable one used to render the same panel, so a
// server that was down or a /api/jobs that failed read as an idle machine.
test("a failed job-list read is not presented as an empty list", async () => {
  globalThis.fetch = async () => {
    throw new TypeError("Failed to fetch");
  };
  await expect(loadJobs()).rejects.toThrow("Failed to fetch");

  render(JobList);
  await tick();
  expect(read(".empty-state h3")).toBe("Request failed");
  expect(read(".empty-state p")).toBe("Could not read the job list.");
});

// The flag must not stay stuck: the socket's init frame carries the same full
// list the HTTP route does, so it is a fresh read and clears the failure.
test("a full job list from either path clears the failed-read state", async () => {
  globalThis.fetch = async () => {
    throw new TypeError("Failed to fetch");
  };
  await loadJobs().catch(() => {});
  expect(ui.jobsLoadFailed).toBe(true);

  const socket = { close() {} };
  connect(() => {}, { factory: () => socket });
  socket.onmessage({ data: JSON.stringify({ type: "init", data: { jobs: [] } }) });
  expect(ui.jobsLoadFailed).toBe(false);

  render(JobList);
  await tick();
  expect(read(".empty-state h3")).toBe("No Active Downloads");
});

test("a transferring job shows percent, live speed and the byte total", async () => {
  render(JobCard, {
    job: {
      id: "job-run",
      repo: "owner/repo",
      status: "running",
      progress: { downloadedBytes: 500, totalBytes: 1000, bytesPerSecond: 2097152, etaSeconds: 240, completedFiles: 1, totalFiles: 3 },
      files: [{ path: "model-00001-of-00002.safetensors", status: "active" }, { path: "model-00002-of-00002.safetensors", status: "active" }]
    }
  });
  await tick();

  expect(read('[data-role="pct"]')).toBe("50.0%");
  expect(read('[data-role="speed"]')).toBe("2 MB/s");
  expect(read('[data-role="last-speed"]')).toBe("");
  expect(read('[data-role="size"]')).toBe("500 B / 1000 B");
  // Two files download at once; naming only one made the other look lost.
  expect(read(".job-file-more")).toBe("+1 parallel");
  expect(node.querySelector(".progress-fill").getAttribute("style")).toContain("max(3px, 50%)");
});

test("a job with no plan total is named, not shown as 0.0% of 0 B", async () => {
  render(JobCard, {
    job: {
      id: "job-failed",
      repo: "owner/repo",
      status: "failed",
      error: "tree API failed: 404 Not Found",
      progress: { downloadedBytes: 268173655, totalBytes: 0, completedFiles: 0, totalFiles: 0 }
    }
  });
  await tick();

  expect(read('[data-role="pct"]')).toBe("unknown");
  expect(read('[data-role="size"]')).toBe("255.8 MB");
  expect(read(".job-error")).toContain("tree API failed");
});

test("a paused job shows its last speed instead of an empty readout", async () => {
  render(JobCard, {
    job: {
      id: "job-paused",
      repo: "owner/repo",
      status: "paused",
      progress: { downloadedBytes: 240000000, totalBytes: 70000000000, bytesPerSecond: 5242880, etaSeconds: 13000, completedFiles: 1, totalFiles: 3 },
      files: [{ path: "model-00001-of-00002.safetensors", status: "complete" }]
    }
  });
  await tick();

  expect(read('[data-role="speed"]')).toBe("");
  expect(read('[data-role="last-speed"]')).toBe("last 5 MB/s");
  // A paused job has no ETA to promise, and 0.3% of 70 GB has to stay visible.
  expect(read('[data-role="eta"]')).toBe("");
  expect(node.querySelector(".progress-fill").getAttribute("style")).toContain("max(3px,");
});

test("the action buttons lock while that job's request is in flight", async () => {
  let release;
  const blocked = new Promise((resolve) => { release = resolve; });
  globalThis.fetch = async () => {
    await blocked;
    return { ok: true, status: 200, json: async () => ({ success: true }) };
  };
  render(JobCard, { job: { id: "job-pause", repo: "owner/repo", status: "paused", progress: {} } });
  await tick();

  const buttons = [...node.querySelectorAll(".job-actions button")];
  expect(buttons.length).toBe(2);
  expect(buttons.every((button) => !button.disabled)).toBe(true);

  buttons[0].click();
  await tick();
  expect([...node.querySelectorAll(".job-actions button")].every((button) => button.disabled)).toBe(true);

  release();
  // The request chain resolves across several microtasks, so a timer is what
  // actually settles it before the buttons are read again.
  await new Promise((resolve) => setTimeout(resolve, 0));
  await tick();
  expect([...node.querySelectorAll(".job-actions button")].every((button) => !button.disabled)).toBe(true);
});

// The jump from a job card is only half done by switching pages: the detail panel
// renders from ui.models.analysis, so a click that merely named the repo landed on
// the empty-state hint instead of the detail it promised.
test("clicking the repo title opens that repo's detail panel", async () => {
  fakeFetch((url) => (String(url).includes("/api/analyze/")
    ? { repo: "owner/repo", type: "gguf", branch: "main", file_count: 2, total_size_human: "10 GB" }
    : { html: "<p>readme</p>" }));
  ui.page = "jobs";
  ui.models.analysis = null;
  render(JobCard, { job: { id: "job-open", repo: "owner/repo", revision: "pr-42", status: "running", progress: {} } });
  await tick();

  node.querySelector(".job-repo-clickable").click();
  // The analyze request settles across microtasks, so a timer is what closes the chain.
  await new Promise((resolve) => setTimeout(resolve, 0));
  await tick();

  expect(ui.page).toBe("models");
  expect(requests).toContain("GET /api/analyze/owner/repo");
  expect(ui.models.analysis && ui.models.analysis.repo).toBe("owner/repo");
  // The README follows the revision the job is really downloading.
  expect(requests.some((entry) => entry.startsWith("GET /api/readme/owner/repo?revision=pr-42"))).toBe(true);
});

test("a repo the hub cannot analyze switches page and names the failure", async () => {
  globalThis.fetch = async () => ({ ok: false, status: 404, json: async () => ({ error: "tree API failed: 404 Not Found" }) });
  ui.page = "jobs";
  ui.models.analysis = null;
  ui.toasts.length = 0;
  render(JobCard, { job: { id: "job-404", repo: "owner/missing", status: "failed", progress: {} } });
  await tick();

  node.querySelector(".job-repo-clickable").click();
  await new Promise((resolve) => setTimeout(resolve, 0));
  await tick();

  expect(ui.page).toBe("models");
  expect(ui.models.analysis).toBe(null);
  expect(ui.toasts.some((item) => String(item.message).includes("404"))).toBe(true);
});

test("a finalizing file names the local stage instead of looking finished at 100%", async () => {
  // The complaint: the card read 100% and looked done while the server was still
  // stitching the multipart parts and hashing the payload. The stage chip is the
  // only thing that says work is still happening, so it has to carry the stage
  // name and how far along it is.
  ui.jobs.set("job-9", {
    id: "job-9",
    repo: "owner/repo",
    status: "running",
    phase: "finalizing",
    progress: { downloadedBytes: 1000, totalBytes: 1000, completedFiles: 0, totalFiles: 1 },
    files: [
      {
        path: "model-00001-of-00002.safetensors",
        status: "finalizing",
        downloaded: 1000,
        totalBytes: 1000,
        stage: "assembling",
        stageBytes: 400,
        stageTotal: 1000
      }
    ]
  });
  render(JobCard, { job: ui.jobs.get("job-9") });
  await tick();

  expect(read('[data-role="pct"]')).toBe("100.0%");
  expect(read('[data-role="stage"]')).toBe("Assembling parts 40%");
});

test("the verify stage drops the assembly counter it no longer owns", async () => {
  ui.jobs.set("job-10", {
    id: "job-10",
    repo: "owner/repo",
    status: "running",
    phase: "finalizing",
    progress: { downloadedBytes: 1000, totalBytes: 1000 },
    files: [{ path: "model.gguf", status: "finalizing", downloaded: 1000, totalBytes: 1000, stage: "verifying" }]
  });
  render(JobCard, { job: ui.jobs.get("job-10") });
  await tick();

  expect(read('[data-role="stage"]')).toBe("Verifying");
});

// The follow-up complaint: the finalize stages moved but said nothing about how
// fast, so a 60 GB re-read gave no hint how long it would take. The server sends
// stage bytes about once a second, so the difference between two readings is the
// local disk rate. It stays inside the chip on purpose: the download speed is
// zero here, and a rate printed next to it read as a download that had stalled.
test("the finalize stage reports its own local rate next to the percentage", async () => {
  const base = {
    id: "job-12",
    repo: "owner/repo",
    status: "running",
    phase: "finalizing",
    progress: { downloadedBytes: 1000, totalBytes: 1000 },
    files: [
      {
        path: "model.gguf",
        status: "finalizing",
        downloaded: 1000,
        totalBytes: 1000,
        stage: "assembling",
        stageBytes: 400,
        stageTotal: 1000
      }
    ]
  };
  ui.jobs.set("job-12", base);
  render(JobList);
  await tick();

  // The first reading only opens the sampling window, so it must not invent a rate.
  expect(read('[data-role="stage"]')).toBe("Assembling parts 40%");

  await new Promise((resolve) => setTimeout(resolve, 600));
  ui.jobs.set("job-12", { ...base, files: [{ ...base.files[0], stageBytes: 900 }] });
  await tick();

  expect(read('[data-role="stage"]')).toMatch(/^Assembling parts 90% \u00B7 [\d.]+ (B|KB)\/s$/);
});

test("a transferring file shows no stage chip", async () => {
  ui.jobs.set("job-11", {
    id: "job-11",
    repo: "owner/repo",
    status: "running",
    progress: { downloadedBytes: 500, totalBytes: 1000 },
    files: [{ path: "a.bin", status: "active", downloaded: 500, totalBytes: 1000 }]
  });
  render(JobCard, { job: ui.jobs.get("job-11") });
  await tick();

  const chip = node.querySelector('[data-role="stage"]');
  expect(chip.style.display).toBe("none");
  expect(chip.textContent.trim()).toBe("");
});