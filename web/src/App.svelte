<script>
  import { untrack } from "svelte";
  import { ui } from "./store.svelte.js";
  import { t } from "./i18n.js";
  import { globalLoading } from "./loading.svelte.js";
  import Shell from "./components/Shell.svelte";
  import Toasts from "./components/Toasts.svelte";

  // Route-level code splitting: the entry bundle carries only the shell, and
  // each page's chunk is fetched the first time that page is opened and cached
  // by the browser afterwards. Nav clicks and the keyboard 1-6 shortcuts keep
  // doing exactly what they always did - flip ui.page - and the effect below
  // follows the flip. There is no top-level await: main.js mounts this
  // component synchronously and the chunk arrives through the pending state.
  const pageLoaders = {
    models: () => import("./pages/Models.svelte"),
    jobs: () => import("./pages/Jobs.svelte"),
    cache: () => import("./pages/Cache.svelte"),
    mirror: () => import("./pages/Mirror.svelte"),
    history: () => import("./pages/History.svelte"),
    settings: () => import("./pages/Settings.svelte")
  };

  const pages = $state({});
  const pageErrors = $state({});
  // Plain Sets for the fetch bookkeeping: an in-flight or failed request must
  // not read or write reactive state from the effect (same rule that keeps
  // loading.svelte.js from re-triggering itself).
  const requested = new Set();
  const failed = new Set();

  function loadPage(page) {
    if (!pageLoaders[page] || pages[page] || requested.has(page) || failed.has(page)) return;
    requested.add(page);
    pageLoaders[page]()
      .then((mod) => {
        pages[page] = mod.default;
      })
      .catch(() => {
        // A chunk fetch can 404 when the server was restarted with a new
        // build while this page still runs the old index. main.js reloads on
        // the socket side (isStaleBuild); here the user gets an explicit
        // retry, because an automatic retry would turn a dead server into a
        // fetch loop.
        failed.add(page);
        pageErrors[page] = true;
      })
      .finally(() => {
        requested.delete(page);
      });
  }

  function retryPage(page) {
    failed.delete(page);
    delete pageErrors[page];
    loadPage(page);
  }

  // Track only ui.page; untrack keeps the reads inside loadPage from making
  // the effect depend on the very state its async callback writes.
  $effect(() => {
    const page = ui.page;
    untrack(() => loadPage(page));
  });

  function onKey(event) {
    // The loading veil blocks the page; keyboard shortcuts must not act
    // underneath it.
    if (globalLoading.visible) return;
    if (event.key === "/" && !event.target.matches("input, select, textarea")) {
      event.preventDefault();
      ui.page = "models";
      queueMicrotask(() => document.querySelector("#analyzeInput")?.focus());
      return;
    }
    const order = ["models", "jobs", "cache", "mirror", "history", "settings"];
    if (/^[1-6]$/.test(event.key) && !event.target.matches("input, select, textarea")) {
      ui.page = order[Number(event.key) - 1];
    }
  }
</script>

<svelte:window onkeydown={onKey} />

<div class="app">
  {#if globalLoading.visible}
    <div class="global-loading" role="status" aria-live="polite">
      <div class="global-loading-card">
        <span class="global-loading-ring" aria-hidden="true"></span>
        <span class="global-loading-text">{t("common.loading", ui.lang)}</span>
      </div>
    </div>
  {/if}
  <Shell />
  <main class="main" id="main-content" tabindex="-1">
    <section class="page active" id={`page-${ui.page}`}>
      {#if pages[ui.page]}
        <svelte:component this={pages[ui.page]} />
      {:else if pageErrors[ui.page]}
        <div class="page-lazy-failed" role="alert">
          <span class="global-loading-text">{t("common.failed", ui.lang)}</span>
          <button type="button" class="btn btn-secondary" onclick={() => retryPage(ui.page)}>{t("common.refresh", ui.lang)}</button>
        </div>
      {:else}
        <div class="page-lazy-loading" role="status" aria-live="polite">
          <span class="global-loading-ring" aria-hidden="true"></span>
          <span class="global-loading-text">{t("common.loading", ui.lang)}</span>
        </div>
      {/if}
    </section>
  </main>
  <Toasts />
</div>
