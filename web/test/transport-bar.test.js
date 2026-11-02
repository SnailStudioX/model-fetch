// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

import { test, expect, beforeEach, afterEach } from "vitest";
import { mount, tick, unmount } from "svelte";
import JobsPage from "../src/pages/Jobs.svelte";
import SettingsPage from "../src/pages/Settings.svelte";
import { ui } from "../src/store.svelte.js";

// The transport knobs (the per-host stream budget, concurrent downloads, retries,
// verification) moved out of the Settings page and into the
// strip above the job list, next to the speed limit, because the server applies
// each one to running jobs. They post partial bodies: the settings API treats every
// field as an optional pointer, so a stepper click must not resend the rest of the
// form. Connections per file is not in the strip: it has no UI control at all and
// stays a config-file/API setting.
let posts = [];
let stored = {};
let rejectNext = false;
let realFetch = null;
let component = null;
let node = null;

function fakeFetch() {
  stored = { maxSpeed: "5120KiB", connections: 8, maxActive: 3, maxStreamsPerHost: 0, retries: 4, verify: "size" };
  globalThis.fetch = async (url, options = {}) => {
    const method = options.method || "GET";
    if (method === "POST") {
      const body = options.body ? JSON.parse(options.body) : null;
      posts.push({ url: String(url), body });
      if (rejectNext) {
        rejectNext = false;
        return { ok: false, status: 400, json: async () => ({ error: "Invalid maxActive", error_detail: { code: "invalid", message: "Invalid maxActive" } }) };
      }
      stored = { ...stored, ...body };
    }
    return { ok: true, status: 200, json: async () => stored };
  };
}

async function settle() {
  await tick();
  await new Promise((resolve) => setTimeout(resolve, 0));
  await tick();
}

function stepper(id) {
  return node.querySelector(`#${id}`).closest(".stepper-control");
}

function bump(id) {
  stepper(id).querySelector('[data-action="increase"]').click();
}

beforeEach(() => {
  realFetch = globalThis.fetch;
  posts = [];
  rejectNext = false;
  ui.jobs.clear();
  ui.jobActions.clear();
  ui.lang = "en";
  ui.page = "jobs";
  ui.settings = { maxSpeed: "5120KiB", connections: 8, maxActive: 3, maxStreamsPerHost: 0, retries: 4, verify: "size" };
  fakeFetch();
  node = document.createElement("div");
  document.body.appendChild(node);
  component = mount(JobsPage, { target: node, props: {} });
});

afterEach(() => {
  globalThis.fetch = realFetch;
  if (component) unmount(component);
  if (node) node.remove();
  component = null;
  node = null;
});

test("the transport knobs sit in the strip above the job list", () => {
  const bar = node.querySelector(".speed-bar");
  expect(bar.querySelector(".speed-row")).toBeTruthy();
  const row = bar.querySelector(".transport-row");
  expect(row).toBeTruthy();
  for (const id of ["maxStreamsJobs", "maxActiveJobs", "retriesJobs", "verifyJobs"]) {
    expect(row.querySelector(`#${id}`)).toBeTruthy();
  }
  // Connections per file left the strip on purpose: it stays a config-only setting.
  expect(row.querySelector("#connectionsJobs")).toBe(null);
});

test("a stepper click posts only the field it changed", async () => {
  bump("maxActiveJobs");
  await settle();
  expect(posts[posts.length - 1].body).toEqual({ maxActive: 4 });

  bump("retriesJobs");
  await settle();
  expect(posts[posts.length - 1].body).toEqual({ retries: 5 });

  bump("maxStreamsJobs");
  await settle();
  expect(posts[posts.length - 1].body).toEqual({ maxStreamsPerHost: 1 });
});

// Zero is a real value here, not a half-typed field: it means "let the stream budget
// follow connections per file", so stepping down to it must post 0 and leave the
// control showing 0 rather than an empty box.
test("the stream budget steps back down to zero", async () => {
  bump("maxStreamsJobs");
  await settle();
  expect(posts[posts.length - 1].body).toEqual({ maxStreamsPerHost: 1 });

  stepper("maxStreamsJobs").querySelector('[data-action="decrease"]').click();
  await settle();
  expect(posts[posts.length - 1].body).toEqual({ maxStreamsPerHost: 0 });
  expect(node.querySelector("#maxStreamsJobs").value).toBe("0");
});

test("the verification mode posts on change", async () => {
  const select = node.querySelector("#verifyJobs");
  expect(select.value).toBe("size");
  select.value = "sha256";
  select.dispatchEvent(new window.Event("change", { bubbles: true }));
  await settle();

  expect(posts[posts.length - 1].body).toEqual({ verify: "sha256" });
});

test("a rejected write snaps the control back to the value the server has", async () => {
  rejectNext = true;
  bump("maxActiveJobs");
  await settle();

  expect(posts[posts.length - 1].body).toEqual({ maxActive: 4 });
  expect(node.querySelector("#maxActiveJobs").value).toBe("3");
});

test("the Settings page no longer carries the transport card", async () => {
  unmount(component);
  component = null;
  node.remove();
  node = null;

  document.body.innerHTML = "";
  ui.page = "settings";
  const settingsNode = document.createElement("div");
  document.body.appendChild(settingsNode);
  component = mount(SettingsPage, { target: settingsNode, props: {} });
  await tick();

  for (const id of ["connections", "maxActive", "maxSpeed", "retries", "verify"]) {
    expect(document.querySelector(`#${id}`)).toBe(null);
  }
  const headings = [...document.querySelectorAll(".settings-card h3")].map((el) => el.textContent.trim());
  expect(headings).not.toContain("Transfer");

  settingsNode.remove();
});