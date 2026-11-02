import { test, expect } from "vitest";
import { mount, tick, unmount } from "svelte";
import Settings from "../src/pages/Settings.svelte";
import { ui, loadSettings } from "../src/store.svelte.js";

const payloads = { "/api/settings": { storageMode: "cache", cacheDir: "C:\\cache" } };

global.fetch = async (url) => ({ ok: true, json: async () => payloads[url] || {} });

test("the Settings page no longer carries the hardware card", async () => {
  document.body.innerHTML = "";
  ui.lang = "en";
  ui.page = "settings";
  await loadSettings();

  const component = mount(Settings, { target: document.body });
  await tick();

  expect(document.querySelector(".hardware-card")).toBe(null);
  expect(document.querySelector(".settings-card")).not.toBe(null);

  unmount(component);
});
