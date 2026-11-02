// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

import { test, expect, beforeEach, afterEach } from "vitest";
import { ui, applyJobUpdate, runJobAction, loadJobs, connect } from "../src/store.svelte.js";

// The reported bug: the server deleted the job, but the card stayed on screen,
// so every later click on it answered 404 "Job not found".
function fakeFetch(handler) {
  globalThis.fetch = async (url, options = {}) => {
    const body = handler(String(url), options.method || "GET");
    return { ok: true, status: 200, json: async () => body };
  };
}

let realFetch;
beforeEach(() => {
  realFetch = globalThis.fetch;
  ui.jobs.clear();
  ui.toasts = [];
});
afterEach(() => {
  globalThis.fetch = realFetch;
});

test("dismiss removes the card the server just deleted", async () => {
  fakeFetch(() => ({ success: true, message: "Job dismissed" }));
  ui.jobs.set("job-1", { id: "job-1", repo: "owner/repo", status: "failed" });

  await runJobAction("job-1", "dismiss");

  expect(ui.jobs.has("job-1")).toBe(false);
});

test("other actions still apply the returned job snapshot", async () => {
  fakeFetch(() => ({ success: true, job: { id: "job-1", status: "cancelled" } }));
  ui.jobs.set("job-1", { id: "job-1", status: "running" });

  await runJobAction("job-1", "cancel");

  expect(ui.jobs.get("job-1").status).toBe("cancelled");
});

test("a queued progress frame cannot resurrect a dismissed card", async () => {
  fakeFetch(() => ({ success: true }));
  ui.jobs.set("job-1", { id: "job-1", repo: "owner/repo", status: "paused" });

  // applyJobUpdate defers to the next frame. Dismissal has to drop that pending
  // frame, otherwise the flush puts the job back after the card is gone.
  applyJobUpdate({ id: "job-1", repo: "owner/repo", status: "running" });
  await runJobAction("job-1", "dismiss");
  await new Promise((resolve) => setTimeout(resolve, 32));

  expect(ui.jobs.has("job-1")).toBe(false);
});

test("refresh drops cards for jobs the server no longer reports", async () => {
  fakeFetch((url) => (url.startsWith("/api/jobs") ? { jobs: [{ id: "live", status: "running" }] } : {}));
  ui.jobs.set("live", { id: "live", status: "running" });
  ui.jobs.set("stale", { id: "stale", status: "failed" });

  await loadJobs();

  expect([...ui.jobs.keys()]).toEqual(["live"]);
});

test("a job_removed frame drops the card in this tab too", () => {
  ui.jobs.set("job-1", { id: "job-1", status: "completed" });
  const socket = { close() {} };
  connect(() => {}, { factory: () => socket });

  socket.onmessage({ data: JSON.stringify({ type: "job_removed", data: { id: "job-1" } }) });

  expect(ui.jobs.has("job-1")).toBe(false);
});
