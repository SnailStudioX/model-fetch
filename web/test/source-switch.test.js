// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

import { test, expect, beforeEach, afterEach } from "vitest";
import { mount, tick, unmount } from "svelte";
import Models from "../src/pages/Models.svelte";
import Settings from "../src/pages/Settings.svelte";
import { ui } from "../src/store.svelte.js";
import { switchSourceBody, activeSource, DEFAULT_SOURCE, DEFAULT_HF_ENDPOINT } from "../src/source.js";

// The reported bug: picking another hub in the source selector changed nothing.
// The server validates source and endpoint together, so a body that names only
// the source is rejected and the control snaps back.

// The endpoint the server falls back to for a source whose setting is empty. The
// Hugging Face one is the mirror, so a switch never lands the user on a host they
// cannot reach.
const DEFAULTS = { hf: DEFAULT_HF_ENDPOINT, modelscope: "https://modelscope.cn" };

let requests = [];
let posts = [];
let server = null;
let rejectSettings = false;

const wait = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

function fakeServer() {
  return {
    source: DEFAULT_SOURCE,
    endpoint: DEFAULTS.modelscope,
    source_defaults: DEFAULTS,
    source_hosts: { hf: ["huggingface.co", "hf.co", "hf-mirror.com"], modelscope: ["modelscope.cn"] },
    storageMode: "cache",
    cacheDir: "C:\\cache"
  };
}

function fakeFetch() {
  globalThis.fetch = async (url, options = {}) => {
    const method = options.method || "GET";
    const path = String(url);
    if (path.startsWith("/api/settings") && method === "POST") {
      const body = JSON.parse(options.body || "{}");
      posts.push(body);
      if (rejectSettings) {
        return { ok: false, status: 400, json: async () => ({ error: "Endpoint does not match source", error_detail: { code: "source_mismatch", message: "endpoint does not match source" } }) };
      }
      if (body.source) server.source = body.source;
      if (body.endpoint !== undefined) server.endpoint = body.endpoint;
      return { ok: true, status: 200, json: async () => ({ success: true, message: "Settings updated" }) };
    }
    if (path.startsWith("/api/settings")) {
      return { ok: true, status: 200, json: async () => ({ ...server }) };
    }
    if (path.startsWith("/api/search")) {
      requests.push(path);
      return { ok: true, status: 200, json: async () => ({ results: [], page: 1, next_url: "" }) };
    }
    requests.push(path);
    return { ok: true, status: 200, json: async () => ({}) };
  };
}

function reset() {
  document.body.innerHTML = "";
  requests = [];
  posts = [];
  rejectSettings = false;
  server = fakeServer();
  fakeFetch();
  ui.lang = "en";
  ui.settings = { ...server };
  ui.sourceEndpoints = {};
  ui.settingsLoadFailed = false;
  ui.toasts = [];
  ui.models.query = "";
  ui.models.results = [];
  ui.models.page = 1;
  ui.models.nextUrl = "";
  ui.models.sort = "recommend";
  ui.models.type = "";
  ui.models.gguf = false;
  ui.models.datasets = false;
  ui.models.analysis = null;
  ui.models.repo = "";
}

beforeEach(reset);
afterEach(() => {
  document.body.innerHTML = "";
  ui.toasts = [];
});

test("the built-in default source is ModelScope", () => {
  expect(DEFAULT_SOURCE).toBe("modelscope");
  expect(activeSource({})).toBe("modelscope");
  expect(activeSource({ source: "ms" })).toBe("modelscope");
});

test("a source switch sends the endpoint that fits it", () => {
  const body = switchSourceBody({ source: "modelscope", endpoint: DEFAULTS.modelscope }, "hf");
  expect(body).toEqual({ source: "hf", endpoint: DEFAULTS.hf });
});

// hf-mirror.com is a host the Hugging Face source claims, so a config that pairs
// it with Hugging Face was written by a switch, not by hand. Carrying it into a
// ModelScope switch is the mismatch the server rejects, which is the "switch does
// nothing" report. A host no source claims is the user's own and must survive.
test("a mirror owned by the other source is replaced by the switch", () => {
  const body = switchSourceBody({ source: "hf", endpoint: "https://hf-mirror.com" }, "modelscope");
  expect(body).toEqual({ source: "modelscope", endpoint: DEFAULTS.modelscope });
});

test("a mirror no source claims is kept across a switch", () => {
  const body = switchSourceBody({ source: "hf", endpoint: "https://hub.internal.example.com" }, "modelscope");
  expect(body).toEqual({ source: "modelscope", endpoint: "https://hub.internal.example.com" });
});

test("the server's host table wins over the built-in one", () => {
  const body = switchSourceBody(
    { source: "hf", endpoint: "https://mirror.example.org", source_hosts: { modelscope: ["mirror.example.org"] } },
    "hf"
  );
  expect(body).toEqual({ source: "hf", endpoint: DEFAULTS.hf });
});

// The endpoint a switch falls back to is the one that source was last used with,
// not the built-in default: a hf-mirror.com user who visits ModelScope and comes
// back should not be dropped onto a host they cannot reach.
test("switching back to a source restores the endpoint configured for it", () => {
  const remembered = { hf: "https://hf-mirror.com", modelscope: DEFAULTS.modelscope };
  const body = switchSourceBody({ source: "modelscope", endpoint: DEFAULTS.modelscope }, "hf", undefined, remembered);
  expect(body).toEqual({ source: "hf", endpoint: "https://hf-mirror.com" });
});

test("a remembered endpoint owned by another source is never reused", () => {
  const remembered = { hf: DEFAULTS.modelscope };
  const body = switchSourceBody({ source: "modelscope", endpoint: DEFAULTS.modelscope }, "hf", undefined, remembered);
  expect(body).toEqual({ source: "hf", endpoint: DEFAULTS.hf });
});

test("the Models strip offers ModelScope first and selects it", async () => {
  ui.page = "models";
  const component = mount(Models, { target: document.body });
  await tick();

  const select = document.querySelector("#modelsSourceSelect");
  expect(select).not.toBe(null);
  expect([...select.options].map((option) => option.value)).toEqual(["modelscope", "hf"]);
  expect(select.value).toBe("modelscope");

  unmount(component);
});

test("switching hub on the Models page saves the pair and re-runs the search", async () => {
  ui.page = "models";
  const component = mount(Models, { target: document.body });
  await wait(300);
  expect(requests.filter((url) => url.startsWith("/api/search")).length).toBe(1);

  const select = document.querySelector("#modelsSourceSelect");
  select.value = "hf";
  select.dispatchEvent(new Event("change", { bubbles: true }));
  await wait(400);

  expect(posts).toEqual([{ source: "hf", endpoint: DEFAULTS.hf }]);
  expect(server.source).toBe("hf");
  expect(ui.settings.source).toBe("hf");
  expect(select.value).toBe("hf");
  const searches = requests.filter((url) => url.startsWith("/api/search"));
  expect(searches.length).toBe(2);
  expect(ui.toasts.some((item) => item.kind === "success" && item.message.includes("Hugging Face"))).toBe(true);

  unmount(component);
});

test("switching away from a Hugging Face mirror sends ModelScope's endpoint", async () => {
  server = { ...server, source: "hf", endpoint: "https://hf-mirror.com" };
  ui.settings = { ...server };
  ui.page = "models";
  const component = mount(Models, { target: document.body });
  await wait(300);

  const select = document.querySelector("#modelsSourceSelect");
  expect(select.value).toBe("hf");

  select.value = "modelscope";
  select.dispatchEvent(new Event("change", { bubbles: true }));
  await wait(400);

  expect(posts).toEqual([{ source: "modelscope", endpoint: DEFAULTS.modelscope }]);
  expect(server.endpoint).toBe(DEFAULTS.modelscope);
  expect(select.value).toBe("modelscope");
  expect(ui.toasts.some((item) => item.kind === "success")).toBe(true);

  unmount(component);
});

test("a round trip through ModelScope lands back on the configured mirror", async () => {
  server = { ...server, source: "hf", endpoint: "https://hf-mirror.com" };
  ui.settings = { ...server };
  ui.sourceEndpoints = { hf: "https://hf-mirror.com" };
  ui.page = "models";
  const component = mount(Models, { target: document.body });
  await wait(300);

  const select = document.querySelector("#modelsSourceSelect");
  select.value = "modelscope";
  select.dispatchEvent(new Event("change", { bubbles: true }));
  await wait(400);
  expect(server.endpoint).toBe(DEFAULTS.modelscope);

  select.value = "hf";
  select.dispatchEvent(new Event("change", { bubbles: true }));
  await wait(400);
  expect(posts[posts.length - 1]).toEqual({ source: "hf", endpoint: "https://hf-mirror.com" });
  expect(server.endpoint).toBe("https://hf-mirror.com");
  expect(select.value).toBe("hf");

  unmount(component);
});

test("a rejected switch snaps the selector back to the server value", async () => {
  rejectSettings = true;
  ui.page = "models";
  const component = mount(Models, { target: document.body });
  await wait(300);

  const select = document.querySelector("#modelsSourceSelect");
  select.value = "hf";
  select.dispatchEvent(new Event("change", { bubbles: true }));
  await wait(400);

  expect(posts.length).toBe(1);
  expect(select.value).toBe("modelscope");
  expect(ui.settings.source).toBe("modelscope");
  expect(ui.toasts.some((item) => item.kind === "error")).toBe(true);

  unmount(component);
});

// A server that predates the saved list still gets a tab per source, and the tab
// bar still switches on its own: the old selector applied a source change without
// waiting for Save, and a control that only moved a form value reads as broken.
test("the Settings hub tabs switch hub without pressing Save", async () => {
  ui.page = "settings";
  const component = mount(Settings, { target: document.body });
  await tick();
  await wait(50);

  const tabs = [...document.querySelectorAll(".hub-tab")];
  expect(tabs.map((tab) => tab.dataset.hub)).toEqual(["modelscope", "hf"]);
  expect(document.querySelector(".hub-tab.is-active").dataset.hub).toBe("modelscope");
  // The live tab has nothing to switch to, so its button is inert.
  expect(document.querySelector("#hubUse").disabled).toBe(true);

  tabs[1].click();
  await tick();
  expect(document.querySelector("#hubUse").disabled).toBe(false);

  document.querySelector("#hubUse").click();
  await wait(400);

  expect(posts).toEqual([{ source: "hf", endpoint: DEFAULTS.hf }]);
  expect(server.source).toBe("hf");
  expect(document.querySelector(".hub-tab.is-active").dataset.hub).toBe("hf");
  expect(document.querySelector("#endpoint").value).toBe(DEFAULTS.hf);
  expect(ui.toasts.some((item) => item.kind === "success" && item.message.includes("Hugging Face"))).toBe(true);

  unmount(component);
});
