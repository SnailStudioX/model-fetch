import { test, expect, afterEach } from "vitest";
import { mount, tick, unmount } from "svelte";
import Cache from "../src/pages/Cache.svelte";
import { ui } from "../src/store.svelte.js";

const cachedEntry = {
  repo: "owner/name",
  owner: "owner",
  name: "name",
  type: "model",
  size: 0,
  sizeHuman: "0 B",
  fileCount: 3,
  downloaded: "2026-01-01",
  downloadStatus: "complete",
  source: "HF cache",
  commit: "",
  tags: []
};

const details = {
  repo: "owner/name",
  owner: "owner",
  name: "name",
  type: "model",
  path: "C:\\cache\\hub\\models--owner--name",
  friendlyPath: "C:\\cache\\models\\owner\\name",
  sizeHuman: "1.2 GB",
  fileCount: 3,
  branch: "main",
  downloadStatus: "complete",
  source: "HF cache",
  files: []
};

const realFetch = globalThis.fetch;
let calls = [];

function stubFetch(deleteBody, listing = [cachedEntry]) {
  calls = [];
  globalThis.fetch = async (url, options = {}) => {
    calls.push({ url, method: options.method || "GET" });
    if ((options.method || "GET") === "DELETE") {
      return { ok: true, status: 200, json: async () => deleteBody };
    }
    return {
      ok: true,
      status: 200,
      json: async () => ({
        repos: listing,
        stats: { totalModels: 0, totalDatasets: 0, totalSize: 0, totalSizeHuman: "0 B", totalFiles: 0 },
        cacheDir: "C:\\cache",
        effectiveHubDir: "C:\\cache\\hub"
      })
    };
  };
}

afterEach(() => {
  globalThis.fetch = realFetch;
});

async function flush() {
  await tick();
  await new Promise((resolve) => setTimeout(resolve, 0));
  await tick();
  await new Promise((resolve) => setTimeout(resolve, 0));
  await tick();
}

async function mountWithDetails() {
  document.body.innerHTML = "";
  ui.lang = "en";
  ui.toasts.length = 0;
  ui.cache.busy = false;
  ui.cache.view = "list";
  ui.cache.filter = "all";
  ui.cache.search = "";
  ui.cache.repos = [cachedEntry];
  ui.cache.stats = { totalModels: 1, totalDatasets: 0, totalSize: 0, totalSizeHuman: "0 B", totalFiles: 0 };
  ui.cache.details = details;
  ui.cache.selection = null;
  ui.cache.outcome = null;
  ui.cache.selectedLocationId = "";
  ui.cache.selectedGroupId = "";
  const component = mount(Cache, { target: document.body });
  await flush();
  return component;
}

function openRepoDeleteConfirm() {
  const button = document.querySelector(".cache-detail-actions .btn-danger");
  expect(button).not.toBeNull();
  button.click();
}

function confirmDeleteButton() {
  const confirmModal = [...document.querySelectorAll(".modal")].find((modal) => modal.querySelector(".delete-confirm"));
  expect(confirmModal).not.toBeNull();
  return confirmModal.querySelector(".btn-danger");
}

test("deleting the whole cache entry closes the repository detail modal", async () => {
  stubFetch({ success: true, message: "Deleted owner/name from cache" });
  const component = await mountWithDetails();
  expect(document.querySelectorAll(".modal-backdrop").length).toBe(1);

  openRepoDeleteConfirm();
  await tick();
  expect(document.querySelectorAll(".modal-backdrop").length).toBe(2);

  confirmDeleteButton().click();
  await flush();

  expect(calls.some((call) => call.method === "DELETE" && call.url.startsWith("/api/cache/owner/name"))).toBe(true);
  expect(ui.cache.details).toBeNull();
  expect(document.querySelectorAll(".modal-backdrop").length).toBe(0);
  unmount(component);
});

test("a partially failed repo delete still closes the modal but is not reported as success", async () => {
  stubFetch({ success: false, message: "Hub repository deleted; friendly view removal failed", errors: ["permission denied"] });
  const component = await mountWithDetails();

  openRepoDeleteConfirm();
  await tick();
  confirmDeleteButton().click();
  await flush();

  expect(ui.cache.details).toBeNull();
  expect(document.querySelectorAll(".modal-backdrop").length).toBe(0);
  const toast = ui.toasts[ui.toasts.length - 1];
  expect(toast.kind).toBe("error");
  expect(toast.message).toContain("friendly view removal failed");
  unmount(component);
});

// The detail modal is a snapshot of one repository, so it has to close when that
// repository is no longer in the listing - deleted in another tab, or dropped by
// a rebuild - instead of staying open with controls that can only fail.
test("the detail modal closes when the repository leaves the listing", async () => {
  const listing = [cachedEntry];
  stubFetch({ success: true }, listing);
  const component = await mountWithDetails();
  expect(document.querySelectorAll(".modal-backdrop").length).toBe(1);

  listing.length = 0;
  document.querySelector(".page-actions .btn-secondary").click();
  await flush();

  expect(ui.cache.details).toBeNull();
  expect(document.querySelectorAll(".modal-backdrop").length).toBe(0);
  unmount(component);
});

test("refreshing with the repository still cached keeps the detail modal open", async () => {
  stubFetch({ success: true });
  const component = await mountWithDetails();

  document.querySelector(".page-actions .btn-secondary").click();
  await flush();

  expect(ui.cache.details).not.toBeNull();
  expect(document.querySelectorAll(".modal-backdrop").length).toBe(1);
  unmount(component);
});
