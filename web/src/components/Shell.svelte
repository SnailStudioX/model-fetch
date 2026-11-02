<script>
  import { ui, loadHardware } from "../store.svelte.js";
  import { t } from "../i18n.js";
  import Icon from "./Icon.svelte";
  const items = ["models", "jobs", "cache", "mirror", "history", "settings"];
  const icons = { models: "search", jobs: "activity", cache: "folder", mirror: "mirror", history: "clock", settings: "gear" };
  const tr = (key) => t(key, ui.lang);
  const activeJobs = $derived([...ui.jobs.values()].filter((job) => job.status !== "completed").length);

  function setLang(lang) {
    ui.lang = lang;
    localStorage.setItem("modelfetch.lang", lang);
    document.documentElement.lang = lang;
  }

  function startResize(event) {
    const sidebar = event.currentTarget.closest(".sidebar");
    if (!sidebar) return;
    const pointerType = event.pointerType;
    const move = (moveEvent) => {
      if (moveEvent.pointerType !== pointerType) return;
      sidebar.style.width = Math.max(200, Math.min(600, moveEvent.clientX)) + "px";
    };
    let stopped = false;
    const stop = () => {
      if (stopped) return;
      stopped = true;
      window.removeEventListener("pointermove", move);
      localStorage.setItem("sidebarWidth", sidebar.style.width.replace("px", ""));
    };
    window.addEventListener("pointermove", move);
    // An interrupted touch or a lost pointer capture never delivers pointerup,
    // so without pointercancel the move handler stays bound and the sidebar
    // keeps following the cursor for the rest of the session.
    window.addEventListener("pointercancel", stop, { once: true });
    window.addEventListener("pointerup", stop, { once: true });
  }

  $effect(() => {
    const saved = localStorage.getItem("sidebarWidth");
    if (saved) document.querySelector(".sidebar")?.style.setProperty("width", saved + "px");
    loadHardware().catch(() => {});
  });
</script>

<a class="skip-link" href="#main-content">{tr("common.skipToContent")}</a>

<aside class="sidebar">
  <div class="sidebar-header">
    <div class="logo">
      <img class="logo-icon" src="icon.svg" alt="" aria-hidden="true" />
      <span class="logo-text">{tr("app.title")}</span>
    </div>
  </div>

  <nav class="nav" aria-label={tr("common.pages")}>
    {#each items as page}
      <button type="button" class="nav-item{ui.page === page ? " active" : ""}" data-page={page} aria-current={ui.page === page ? "page" : undefined} onclick={() => (ui.page = page)}>
        <span class="nav-icon"><Icon name={icons[page]} size={18} /></span>
        <span>{tr("nav." + page)}</span>
        {#if page === "jobs" && activeJobs > 0}
          <span class="nav-badge">{activeJobs}</span>
        {/if}
      </button>
    {/each}
  </nav>

  <div class="sidebar-footer">
    <div class="lang-switch" role="group" aria-label={tr("common.language")}>
      <button type="button" class="lang-btn{ui.lang === "zh" ? " lang-btn-active" : ""}" aria-pressed={ui.lang === "zh"} title={tr("common.langZh")} onclick={() => setLang("zh")}>中</button>
      <button type="button" class="lang-btn{ui.lang === "en" ? " lang-btn-active" : ""}" aria-pressed={ui.lang === "en"} title={tr("common.langEn")} onclick={() => setLang("en")}>En</button>
    </div>
    <div class="connection-status">
      <span class="status-indicator{ui.connected ? " connected" : ""}"></span>
      <span class="status-text">{ui.connected ? tr("common.connected") : tr("common.reconnecting")}</span>
      <span class="version">{ui.version ? "v" + ui.version : ""}</span>
    </div>
  </div>
  <div class="sidebar-resize-handle" onpointerdown={startResize}></div>
</aside>