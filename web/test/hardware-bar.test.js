import { test, expect } from "vitest";
import { mount, tick, unmount } from "svelte";
import Models from "../src/pages/Models.svelte";
import { ui, loadSettings, loadHardware } from "../src/store.svelte.js";

const posts = [];

const payloads = {
  "/api/settings": { storageMode: "cache", cacheDir: "C:\\cache", effectiveHubDir: "C:\\cache", hardware: { gpu_count: 2, gpus: [{ name: "NVIDIA GeForce RTX 3090" }, { name: "AMD Radeon RX 580 2048SP" }] } },
  "/api/hardware": {
    gpus: [
      { index: 1, name: "NVIDIA GeForce RTX 3090", vram_gb: 24, bandwidth_gbps: 936, cores: 10496 },
      { index: 2, name: "AMD Radeon RX 580 2048SP", vram_gb: 8, bandwidth_gbps: 256, cores: 2304 }
    ],
    detected_gpus: [
      { name: "NVIDIA GeForce RTX 3090", vram_gb: 24, bandwidth_gbps: 936, cores: 10496 },
      { name: "AMD Radeon RX 580 2048SP", vram_gb: 8, bandwidth_gbps: 256, cores: 2304 }
    ],
    catalog_entries: { "rtx 4070": { vram_gb: 12, bandwidth_gbps: 716, cores: 5888 } },
    effective: { gpu_count: 2, system_ram_gb: 64, context_length: 8192 },
    sources: {}
  },
  "/api/diskfree": { free: 1000, total: 2000 },
  "/api/search": { results: [], page: 1, next_url: "" }
};

global.fetch = async (url, options) => {
  if (options?.method === "POST") posts.push(JSON.parse(options.body));
  return { ok: true, json: async () => payloads[url] || {} };
};

const wait = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

test("picking a different card saves the picked name", async () => {
  document.body.innerHTML = "";
  ui.lang = "en";
  ui.page = "models";
  ui.models.query = "";
  ui.models.page = 1;
  ui.models.results = [];
  posts.length = 0;
  await loadSettings();
  await loadHardware();

  const component = mount(Models, { target: document.body });
  await tick();
  await wait(200);

  const input = document.querySelector('input[aria-label="GPU 2"]');
  expect(input).not.toBe(null);

  input.dispatchEvent(new Event("focus"));
  await tick();
  input.value = "RTX 4070";
  input.dispatchEvent(new Event("input", { bubbles: true }));
  await tick();

  const option = document.querySelector(".gpu-combobox-list button");
  expect(option.textContent.trim().startsWith("RTX 4070")).toBe(true);
  option.click();

  await wait(900);

  const saved = posts.find((body) => body.hardware?.gpus?.[1]?.name);
  expect(saved?.hardware?.gpus?.[1]?.name).toBe("RTX 4070");
  unmount(component);
});
test("typing a RAM value in the bar saves it as an override", async () => {
  document.body.innerHTML = "";
  ui.lang = "en";
  ui.page = "models";
  ui.models.query = "";
  ui.models.page = 1;
  ui.models.results = [];
  posts.length = 0;
  await loadSettings();
  await loadHardware();

  const component = mount(Models, { target: document.body });
  await tick();
  await wait(200);

  const input = document.querySelector('.hw-head-memory input');
  expect(input).not.toBe(null);
  expect(input.getAttribute("type")).toBe("text");
  expect(input.value).toBe("64");

  input.value = "96";
  input.dispatchEvent(new Event("input", { bubbles: true }));

  await wait(900);
  const saved = posts.find((body) => body.hardware?.system_ram_gb === 96);
  expect(saved?.hardware?.system_ram_gb).toBe(96);
  unmount(component);
});

test("text that is not a number does not become an override", async () => {
  document.body.innerHTML = "";
  ui.lang = "en";
  ui.page = "models";
  ui.models.query = "";
  ui.models.page = 1;
  ui.models.results = [];
  posts.length = 0;
  await loadSettings();
  await loadHardware();

  const component = mount(Models, { target: document.body });
  await tick();
  await wait(200);

  const input = document.querySelector('.hw-head-memory input');
  input.value = "64G";
  input.dispatchEvent(new Event("input", { bubbles: true }));

  await wait(900);
  const saved = posts.find((body) => body.hardware?.system_ram_gb !== undefined);
  expect(saved).toBeUndefined();
  unmount(component);
});

test("the context length chip edits the same override block", async () => {
  document.body.innerHTML = "";
  ui.lang = "en";
  ui.page = "models";
  ui.models.query = "";
  ui.models.results = [];
  ui.models.page = 1;
  posts.length = 0;
  await loadSettings();
  await loadHardware();

  const component = mount(Models, { target: document.body });
  await tick();
  await wait(200);

  const input = document.querySelector(".context-chip input");
  expect(input).not.toBe(null);
  expect(input.value).toBe("8192");

  input.value = "4096";
  input.dispatchEvent(new Event("input", { bubbles: true }));

  await wait(900);
  const saved = posts.find((body) => body.hardware?.context_length === 4096);
  expect(saved?.hardware?.context_length).toBe(4096);
  unmount(component);
});
