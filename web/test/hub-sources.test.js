// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

import { test, expect, beforeEach, afterEach } from "vitest";
import { mount, tick, unmount } from "svelte";
import Models from "../src/pages/Models.svelte";
import Settings from "../src/pages/Settings.svelte";
import { ui } from "../src/store.svelte.js";
import { DEFAULT_HF_ENDPOINT, hubSourceLabel } from "../src/source.js";

// Hub access sources are saved entries, not source ids: the Settings card is one
// tab per entry, the Models page lists the same entries, and both address them by
// id so two Hugging Face mirrors can exist side by side.

let posts = [];
let requests = [];
let server = null;

const wait = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

function fakeServer() {
  return {
    source: "modelscope",
    endpoint: "",
    active_hub_source: "modelscope",
    hub_sources: [
      { id: "modelscope", name: "ModelScope", source: "modelscope", endpoint: "", token: "" },
      { id: "hf", name: "Hugging Face", source: "hf", endpoint: DEFAULT_HF_ENDPOINT, token: "********abcd" }
    ],
    source_defaults: { hf: DEFAULT_HF_ENDPOINT, modelscope: "https://modelscope.cn" },
    source_hosts: { hf: ["huggingface.co", "hf.co", "hf-mirror.com"], modelscope: ["modelscope.cn"] },
    storageMode: "cache",
    cacheDir: "C:\\cache"
  };
}

// applySettings mirrors the server: the list is replaced wholesale, a credential
// survives unless the request authored one, and the live source/endpoint always
// describes the active entry.
function applySettings(body) {
  if (Array.isArray(body.hubSources)) {
    const previous = server.hub_sources;
    server.hub_sources = body.hubSources.map((entry) => {
      const stored = previous.find((item) => item.id === entry.id);
      return {
        id: entry.id,
        name: entry.name || "",
        source: entry.source,
        endpoint: entry.endpoint || "",
        token: Object.prototype.hasOwnProperty.call(entry, "token") ? entry.token : stored ? stored.token : ""
      };
    });
  }
  if (body.activeHubSource !== undefined) server.active_hub_source = body.activeHubSource;
  if (body.token !== undefined) server.token = body.token;
  const active = server.hub_sources.find((entry) => entry.id === server.active_hub_source) || server.hub_sources[0];
  if (active) {
    server.source = active.source;
    server.endpoint = active.endpoint;
    if (body.token !== undefined) active.token = body.token;
  }
}

function fakeFetch() {
  globalThis.fetch = async (url, options = {}) => {
    const method = options.method || "GET";
    const path = String(url);
    if (path.startsWith("/api/settings") && method === "POST") {
      const body = JSON.parse(options.body || "{}");
      posts.push(body);
      applySettings(body);
      return { ok: true, status: 200, json: async () => ({ success: true, message: "Settings updated" }) };
    }
    if (path.startsWith("/api/settings")) {
      return { ok: true, status: 200, json: async () => ({ ...server, hub_sources: server.hub_sources.map((entry) => ({ ...entry })) }) };
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
  server = fakeServer();
  fakeFetch();
  ui.lang = "en";
  ui.settings = { ...server, hub_sources: server.hub_sources.map((entry) => ({ ...entry })) };
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

async function openSettings() {
  ui.page = "settings";
  const component = mount(Settings, { target: document.body });
  await tick();
  await wait(50);
  return component;
}

const tabIds = () => [...document.querySelectorAll(".hub-tab")].map((tab) => tab.dataset.hub);
const tabNames = () => [...document.querySelectorAll(".hub-tab-name")].map((name) => name.textContent);

beforeEach(reset);
afterEach(() => {
  document.body.innerHTML = "";
  ui.toasts = [];
});

test("the hub card renders one tab per saved source", async () => {
  const component = await openSettings();

  expect(tabIds()).toEqual(["modelscope", "hf"]);
  // The seeded Hugging Face entry points at the endpoint its source uses by
  // default, so its label stays a plain name. The host suffix only marks an entry
  // that downloads somewhere else; that case is asserted at the end of this file.
  expect(tabNames()).toEqual(["ModelScope", "Hugging Face"]);
  expect(document.querySelector(".hub-tab.is-active").dataset.hub).toBe("modelscope");
  expect(document.querySelectorAll(".hub-tab-badge").length).toBe(1);
  expect(document.querySelector(".hub-card-add")).not.toBe(null);

  unmount(component);
});

test("the card body edits the picked tab, not the live one", async () => {
  const component = await openSettings();

  expect(document.querySelector("#endpoint").value).toBe("");
  document.querySelectorAll(".hub-tab")[1].click();
  await tick();
  expect(document.querySelector("#endpoint").value).toBe(DEFAULT_HF_ENDPOINT);
  expect(document.querySelector("#hfToken").value).toBe("********abcd");

  unmount(component);
});

test("the plus button adds a tab and Save writes the whole list", async () => {
  const component = await openSettings();

  // A new tab starts from the one in front of the user: the hub they are looking
  // at is the hub they are duplicating to point somewhere else.
  document.querySelectorAll(".hub-tab")[1].click();
  await tick();
  document.querySelector(".hub-card-add").click();
  await tick();
  expect(tabIds()).toEqual(["modelscope", "hf", "hf-2"]);
  expect(document.querySelector(".hub-tab.is-active").dataset.hub).toBe("hf-2");
  expect(document.querySelector("#endpoint").value).toBe(DEFAULT_HF_ENDPOINT);

  document.querySelector(".page-actions .btn-primary").click();
  await wait(400);

  const sent = posts[0].hubSources;
  expect(sent.map((entry) => entry.id)).toEqual(["modelscope", "hf", "hf-2"]);
  expect(sent[2]).toEqual({ id: "hf-2", name: "", source: "hf", endpoint: DEFAULT_HF_ENDPOINT });
  // An untouched mask is not authored, so the stored credential survives the save.
  expect(Object.prototype.hasOwnProperty.call(sent[1], "token")).toBe(false);
  expect(server.hub_sources.find((entry) => entry.id === "hf").token).toBe("********abcd");
  expect(tabIds()).toEqual(["modelscope", "hf", "hf-2"]);

  unmount(component);
});

test("an authored token is sent for its own tab only", async () => {
  const component = await openSettings();

  document.querySelectorAll(".hub-tab")[1].click();
  await tick();
  const token = document.querySelector("#hfToken");
  token.value = "";
  token.dispatchEvent(new Event("input", { bubbles: true }));
  await tick();

  document.querySelector(".page-actions .btn-primary").click();
  await wait(400);

  const sent = posts[0].hubSources;
  expect(sent.find((entry) => entry.id === "hf").token).toBe("");
  expect(Object.prototype.hasOwnProperty.call(sent.find((entry) => entry.id === "modelscope"), "token")).toBe(false);
  expect(server.hub_sources.find((entry) => entry.id === "hf").token).toBe("");

  unmount(component);
});

test("use as current source names the tab and the live pair follows it", async () => {
  const component = await openSettings();

  document.querySelectorAll(".hub-tab")[1].click();
  await tick();
  document.querySelector("#hubUse").click();
  await wait(400);

  expect(posts[0].activeHubSource).toBe("hf");
  expect(server.active_hub_source).toBe("hf");
  expect(server.source).toBe("hf");
  expect(server.endpoint).toBe(DEFAULT_HF_ENDPOINT);
  expect(document.querySelector(".hub-tab.is-active").dataset.hub).toBe("hf");
  expect(ui.toasts.some((item) => item.kind === "success" && item.message.includes("Hugging Face"))).toBe(true);

  unmount(component);
});

test("the live tab cannot be removed, and a saved tab can", async () => {
  const component = await openSettings();

  document.querySelector('[data-hub-remove="modelscope"]').click();
  await tick();
  expect(tabIds()).toEqual(["modelscope", "hf"]);
  expect(ui.toasts.some((item) => item.kind === "error")).toBe(true);

  document.querySelector('[data-hub-remove="hf"]').click();
  await tick();
  expect(tabIds()).toEqual(["modelscope"]);

  unmount(component);
});

test("the Models page lists the saved sources and switches by id", async () => {
  ui.page = "models";
  const component = mount(Models, { target: document.body });
  await wait(300);

  const select = document.querySelector("#modelsSourceSelect");
  expect([...select.options].map((option) => option.value)).toEqual(["modelscope", "hf"]);
  expect([...select.options].map((option) => option.textContent)).toEqual(["ModelScope", "Hugging Face"]);
  expect(select.value).toBe("modelscope");

  select.value = "hf";
  select.dispatchEvent(new Event("change", { bubbles: true }));
  await wait(400);

  expect(posts).toEqual([{ activeHubSource: "hf" }]);
  expect(server.active_hub_source).toBe("hf");
  expect(ui.settings.active_hub_source).toBe("hf");
  expect(select.value).toBe("hf");
  expect(requests.filter((url) => url.startsWith("/api/search")).length).toBe(2);

  unmount(component);
});

// Two Hugging Face tabs that download through different hosts have to be tellable
// apart, so an entry that leaves its source's default host carries that host.
test("a source on a non-default host is labelled with it", () => {
  const settings = { source_defaults: { hf: DEFAULT_HF_ENDPOINT, modelscope: "https://modelscope.cn" } };
  expect(hubSourceLabel({ name: "Hugging Face", source: "hf", endpoint: DEFAULT_HF_ENDPOINT }, settings)).toBe("Hugging Face");
  expect(hubSourceLabel({ name: "Hugging Face", source: "hf", endpoint: "https://huggingface.co" }, settings)).toBe("Hugging Face · huggingface.co");
});
