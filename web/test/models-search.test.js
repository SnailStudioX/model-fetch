import { test, expect } from "vitest";
import { mount, tick, unmount } from "svelte";
import Models from "../src/pages/Models.svelte";
import { ui } from "../src/store.svelte.js";

const requests = [];

// Captured before any test mutates the shared store, so this is the store default.
const defaultGguf = ui.models.gguf;

const baseSearchPayload = {
  results: [{ id: "TheBloke/Mistral-7B-Instruct-v0.2-GGUF", author: "TheBloke", downloads: 1000, likes: 10, tags: ["custom_tag:gguf"] }],
  page: 1,
  next_url: ""
};
let searchPayload = baseSearchPayload;

global.fetch = async (url, options) => {
  requests.push(url);
  if (url.startsWith("/api/search")) return { ok: true, json: async () => searchPayload };
  return { ok: true, json: async () => ({}) };
};

const wait = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

function resetModelsState() {
  document.body.innerHTML = "";
  requests.length = 0;
  ui.lang = "en";
  ui.page = "models";
  ui.models.query = "";
  ui.models.results = [];
  ui.models.page = 1;
  ui.models.nextUrl = "";
  ui.models.warnings = [];
  ui.models.sort = "recommend";
  ui.models.type = "";
  ui.models.gguf = false;
  ui.models.datasets = false;
  searchPayload = baseSearchPayload;
}

test("entering the page runs the default search with no query", async () => {
  resetModelsState();

  const component = mount(Models, { target: document.body });
  await wait(300);

  const search = requests.find((url) => url.startsWith("/api/search"));
  expect(search).toBeTruthy();
  expect(search).not.toContain("q=");
  expect(document.querySelectorAll(".model-row").length).toBe(1);

  unmount(component);
});

test("changing a filter re-runs the search for the default listing", async () => {
  resetModelsState();

  const component = mount(Models, { target: document.body });
  await wait(300);
  expect(requests.filter((url) => url.startsWith("/api/search")).length).toBe(1);

  ui.models.gguf = true;
  await wait(600);

  const searches = requests.filter((url) => url.startsWith("/api/search"));
  expect(searches.length).toBe(2);
  expect(searches[1]).toContain("filter=gguf");

  unmount(component);
});
test("GGUF is on by default, so the default listing is GGUF-only", async () => {
  expect(defaultGguf).toBe(true);

  document.body.innerHTML = "";
  requests.length = 0;
  ui.lang = "en";
  ui.page = "models";
  ui.models.query = "";
  ui.models.results = [];
  ui.models.page = 1;
  ui.models.nextUrl = "";
  ui.models.sort = "recommend";
  ui.models.type = "";
  ui.models.gguf = true;
  ui.models.datasets = false;

  const component = mount(Models, { target: document.body });
  await wait(300);

  const search = requests.find((url) => url.startsWith("/api/search"));
  expect(search).toBeTruthy();
  expect(search).toContain("filter=gguf");
  expect(document.querySelector("#modelsGguf").checked).toBe(true);

  unmount(component);
});

test("the GGUF model filter does not narrow a dataset search", async () => {
  document.body.innerHTML = "";
  requests.length = 0;
  ui.lang = "en";
  ui.page = "models";
  ui.models.query = "";
  ui.models.results = [];
  ui.models.page = 1;
  ui.models.nextUrl = "";
  ui.models.sort = "recommend";
  ui.models.type = "";
  ui.models.gguf = true;
  ui.models.datasets = true;

  const component = mount(Models, { target: document.body });
  await wait(300);

  const search = requests.find((url) => url.startsWith("/api/search"));
  expect(search).toBeTruthy();
  expect(search).toContain("datasets=true");
  expect(search).not.toContain("filter=gguf");

  unmount(component);
});

// Rows come back looking ordinary either way, so a hub that answered in another
// order, or with a shorter page than asked, has to say so where the results are.
test("a search the hub answered differently says so above the results", async () => {
  resetModelsState();
  searchPayload = {
    ...baseSearchPayload,
    warnings: ['modelscope has no "trendingScore" search order, so results come back ordered by downloads', "modelscope caps a search page at 50 results; 100 were requested"]
  };

  const component = mount(Models, { target: document.body });
  await wait(300);

  const banner = document.querySelector("#modelsSearchWarnings");
  expect(banner).toBeTruthy();
  expect(banner.textContent).toContain("trendingScore");
  expect(banner.textContent).toContain("caps a search page");
  // The notice sits alongside the results, not instead of them.
  expect(document.querySelectorAll(".model-row").length).toBe(1);

  unmount(component);
});

// A notice about one query must not survive the next query that was answered as
// asked; a stale warning is its own kind of wrong answer.
test("warnings do not outlive the search they describe", async () => {
  resetModelsState();
  searchPayload = { ...baseSearchPayload, warnings: ["modelscope caps a search page at 50 results; 100 were requested"] };
  let component = mount(Models, { target: document.body });
  await wait(300);
  expect(document.querySelector("#modelsSearchWarnings")).toBeTruthy();
  unmount(component);

  resetModelsState();
  component = mount(Models, { target: document.body });
  await wait(300);
  expect(document.querySelector("#modelsSearchWarnings")).toBeNull();
  unmount(component);
});
