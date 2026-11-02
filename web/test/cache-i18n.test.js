import { test, expect, afterEach } from "vitest";
import { mount, tick, unmount } from "svelte";
import Cache from "../src/pages/Cache.svelte";
import { ui } from "../src/store.svelte.js";

const details = {
  repo: "owner/name",
  owner: "owner",
  name: "name",
  type: "model",
  path: "C:\\cache\\hub",
  friendlyPath: "C:\\cache\\view",
  sizeHuman: "0 B",
  fileCount: 0,
  branch: "main",
  downloadStatus: "unknown",
  source: "HF cache",
  files: []
};

const realFetch = globalThis.fetch;

// The Cache page reads its listing on entry, so the mount now performs a
// request; the stub answers it so the assertions below see a settled list.
function stubCacheApi() {
  globalThis.fetch = async () => ({
    ok: true,
    status: 200,
    json: async () => ({
      repos: [{ repo: "owner/name", owner: "owner", name: "name", type: "model", size: 0, sizeHuman: "0 B", fileCount: 0, downloaded: "2026-01-01", downloadStatus: "unknown", source: "HF cache", commit: "", tags: [] }],
      stats: { totalModels: 1, totalDatasets: 0, totalSize: 0, totalSizeHuman: "0 B", totalFiles: 0 },
      cacheDir: "C:\\cache",
      effectiveHubDir: "C:\\cache\\hub"
    })
  });
}

afterEach(() => {
  globalThis.fetch = realFetch;
});

async function flush() {
  await tick();
  await new Promise((resolve) => setTimeout(resolve, 0));
  await tick();
}

async function mountCache(lang) {
  document.body.innerHTML = "";
  ui.lang = lang;
  ui.cache.busy = false;
  ui.cache.view = "list";
  ui.cache.filter = "all";
  ui.cache.search = "";
  ui.cache.stats = { totalModels: 1, totalDatasets: 0, totalSize: 0, totalSizeHuman: "0 B", totalFiles: 0 };
  ui.cache.repos = [{ repo: "owner/name", owner: "owner", name: "name", type: "model", size: 0, sizeHuman: "0 B", fileCount: 0, downloaded: "2026-01-01", downloadStatus: "unknown", source: "HF cache", commit: "", tags: [] }];
  ui.cache.details = details;
  ui.cache.selection = null;
  ui.cache.outcome = null;
  ui.cache.selectedLocationId = "";
  ui.cache.selectedGroupId = "";
  stubCacheApi();
  const component = mount(Cache, { target: document.body });
  await flush();
  return component;
}

test("the cache detail modal follows the active language", async () => {
  const zh = await mountCache("zh");
  const modal = document.querySelector(".modal");
  expect(modal).not.toBeNull();
  const text = modal.textContent;
  for (const label of ["仓库详情", "下载信息", "路径", "缓存（HF 格式）", "友好视图"]) {
    expect(text).toContain(label);
  }
  for (const english of ["Download Info", "Paths", "Friendly view", "Repository Details"]) {
    expect(text).not.toContain(english);
  }
  unmount(zh);

  const en = await mountCache("en");
  const enText = document.querySelector(".modal").textContent;
  expect(enText).toContain("Download Info");
  expect(enText).toContain("Friendly view");
  unmount(en);
});

test("the cache table header follows the active language", async () => {
  const component = await mountCache("zh");
  const header = document.querySelector(".cache-table-header");
  expect(header).not.toBeNull();
  expect(header.textContent).toContain("已下载");
  expect(header.textContent).not.toContain("Downloaded");
  unmount(component);
});
