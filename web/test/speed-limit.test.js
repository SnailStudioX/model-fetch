// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

import { test, expect, beforeEach, afterEach } from "vitest";
import { mount, tick, unmount } from "svelte";
import JobsPage from "../src/pages/Jobs.svelte";
import { ui } from "../src/store.svelte.js";

// The reported bug: the speed limit could not be adjusted. The UI posted the cap as a
// rate string ("655360 B/s"), the settings API rejected it with 400 Invalid maxSpeed,
// and the control snapped straight back to unlimited. The server grammar is
// "<number><unit>" with B/KB/MB/GB/TB or KiB/MiB/GiB/TiB, no "/s" suffix
// (see pkg/hfdownloader ParseSizeStrict).
const SERVER_SIZE_GRAMMAR = /^\s*\d+(\.\d+)?\s*(B|KB|MB|GB|TB|KiB|MiB|GiB|TiB)?\s*$/i;

let posts = [];
let stored = {};
let realFetch = null;
let component = null;
let node = null;

function fakeFetch() {
  stored = { maxSpeed: "", connections: 8, maxActive: 3 };
  globalThis.fetch = async (url, options = {}) => {
    const method = options.method || "GET";
    if (method === "POST") {
      const body = options.body ? JSON.parse(options.body) : null;
      posts.push({ url: String(url), body });
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

function preset(label) {
  return [...node.querySelectorAll(".speed-btn")].find((btn) => btn.textContent.trim() === label);
}

beforeEach(() => {
  realFetch = globalThis.fetch;
  posts = [];
  ui.jobs.clear();
  ui.jobActions.clear();
  ui.lang = "zh";
  ui.settings = { maxSpeed: "", connections: 8, maxActive: 3 };
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

test("a preset click posts a byte rate the settings API accepts", async () => {
  preset("5").click();
  await settle();

  const post = posts.find((entry) => entry.url === "/api/settings");
  expect(post).toBeTruthy();
  expect(post.body.maxSpeed).toBe("5120KiB");
  expect(SERVER_SIZE_GRAMMAR.test(post.body.maxSpeed)).toBe(true);
});

test("the accepted cap lights up the preset it belongs to", async () => {
  preset("25").click();
  await settle();

  // The read-back is the half that used to fail: the stored "25600KiB" was parsed as
  // unlimited, so no preset ever looked selected after a save.
  expect(preset("25").className).toContain("active");
  expect(node.querySelector(".speed-current").textContent).toContain("/ 25");
});

test("the unlimited preset clears the cap", async () => {
  preset("25").click();
  await settle();
  preset("\u221e").click();
  await settle();

  const post = posts[posts.length - 1];
  expect(post.url).toBe("/api/settings");
  expect(post.body.maxSpeed).toBe("");
  expect(preset("\u221e").className).toContain("active");
});

test("a typed limit is posted on Enter", async () => {
  const field = node.querySelector("#maxSpeedJobs");
  field.value = "12";
  field.dispatchEvent(new window.KeyboardEvent("keydown", { key: "Enter", bubbles: true }));
  await settle();

  const post = posts.find((entry) => entry.url === "/api/settings");
  expect(post.body.maxSpeed).toBe("12288KiB");
  expect(SERVER_SIZE_GRAMMAR.test(post.body.maxSpeed)).toBe(true);
});