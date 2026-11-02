<script>
  import { ui, loadSettings, saveSettings, switchSource, toast } from "../store.svelte.js";
  import { t, tf } from "../i18n.js";
  import { activeHubId, hubSourceLabel, hubSources, normalizeSource, sourceDefaults, sourceName, DEFAULT_SOURCE, DEFAULT_HF_ENDPOINT, SOURCE_NAMES, SOURCE_ORDER } from "../source.js";
  import Icon from "../components/Icon.svelte";

  const tr = (key) => t(key, ui.lang);
  const fileName = (path) => String(path || "").split(/[\\/]/).pop() || path;
  const hubSortFallbacks = { modelscope: { trendingScore: "downloads" } };

  let form = $state({
    proxyUrl: "", proxyUsername: "", proxyPassword: "", proxyNoProxy: "", proxyNoEnvProxy: false
  });
  // Hub access sources are one list: the tabs below, the Models page selector and
  // the live source/endpoint/token all read the same saved entries. The baseline
  // is per entry, so an untouched mask preserves that hub's own credential.
  let hubs = $state([]);
  let hubPick = $state("");
  let hubSwitching = $state(false);
  let tokenReset = $state(false);
  // The transport knobs - concurrent downloads, max speed, retries, verification -
  // now live on the Jobs page, where they are applied to running jobs instantly, so
  // this form no longer carries them. Connections per file has no control at all
  // anymore (config file / API only), but Reset still returns it to its default by
  // authoring the default into the next save, the same way it restores the knobs
  // that do have controls.
  let transportReset = $state(null);
  // The storage fields - friendly cache root, local download folder, local scan
  // folders and the per-type folders - moved to the pages that act on them: the
  // Jobs page writes downloads, the Cache page reads the roots. Reset still has
  // to be able to return them to defaults, so it authors the defaults into the
  // next save instead of leaving them out of every save.
  let storageReset = $state(null);
  let showToken = $state(false);
  let showProxyPassword = $state(false);

  const settings = $derived(ui.settings || {});

  const hubDefaults = $derived(sourceDefaults(settings));

  $effect(() => {
    if (!settings || ui.settingsLoadFailed) return;
    hubs = hubDraft(hubSources(settings));
    hubPick = activeHubId(settings);
    form.proxyUrl = settings.proxy?.url || "";
    form.proxyUsername = settings.proxy?.username || "";
    form.proxyNoProxy = settings.proxy?.noProxy || "";
    form.proxyNoEnvProxy = Boolean(settings.proxy?.noEnvProxy);
  });

  // hubDraft copies the saved list into editable tabs. tokenBaseline keeps the
  // masked value the server sent, so an untouched token field means "keep it".
  function hubDraft(list) {
    return (list || []).map((entry) => ({
      id: entry.id,
      name: entry.name || "",
      source: entry.source,
      endpoint: entry.endpoint || "",
      token: entry.token || "",
      tokenBaseline: entry.token || "",
      active: Boolean(entry.active)
    }));
  }

  const selectedHub = $derived(hubs.find((entry) => entry.id === hubPick) || hubs[0] || null);

  // isLiveHub marks the entry the server actually downloads with. A server that
  // does not report the list yet has one live source, and that is the entry the
  // tabs belong to.
  function isLiveHub(entry) {
    if (!entry) return false;
    const live = String(settings.active_hub_source || "");
    if (live) return entry.id === live;
    return entry.id === activeHubId(settings);
  }

  // A new tab starts as a copy of the one in front of the user, because that is
  // the hub they are configuring: an empty second tab is a form to fill, and a
  // duplicated one is a mirror to point somewhere else.
  function addHubSource() {
    const base = normalizeSource(selectedHub?.source || DEFAULT_SOURCE);
    const taken = new Set(hubs.map((entry) => entry.id));
    let id = base;
    for (let n = 2; taken.has(id); n += 1) id = base + "-" + n;
    hubs = [...hubs, {
      id,
      name: "",
      source: base,
      endpoint: base === "hf" ? DEFAULT_HF_ENDPOINT : "",
      token: "",
      tokenBaseline: "",
      active: false
    }];
    hubPick = id;
    toast(tr("settings.hubAdded"), "info");
  }

  // Removing a tab is only local until Save: the server replaces the whole list,
  // so the entry that is live cannot be dropped out from under the running jobs.
  function removeHubSource(entry) {
    if (!entry) return;
    if (hubs.length <= 1) {
      toast(tr("settings.hubRemoveLast"), "error");
      return;
    }
    if (isLiveHub(entry)) {
      toast(tr("settings.hubRemoveActive"), "error");
      return;
    }
    hubs = hubs.filter((item) => item.id !== entry.id);
    if (hubPick === entry.id) hubPick = hubs[0].id;
    toast(tr("settings.hubRemoved"), "info");
  }

  // hubRequestBody sends one tab. The token is only authored when it differs from
  // the mask the server sent: leaving it out keeps the stored credential, "" clears
  // it, and a new tab has nothing stored to keep.
  function hubRequestBody(entry) {
    const body = {
      id: entry.id,
      name: entry.name || "",
      source: normalizeSource(entry.source),
      endpoint: entry.endpoint || ""
    };
    if (entry.token !== entry.tokenBaseline) body.token = entry.token;
    return body;
  }

  // "Use as current source" is applied on its own, like the old source selector: a
  // tab that only moved a local form value is what the user reads as a switch that
  // does nothing. The whole draft travels with it, so edits typed in another tab
  // are not silently dropped by the switch.
  async function useActiveHub(entry) {
    if (!entry || hubSwitching) return;
    hubSwitching = true;
    try {
      if (Array.isArray(settings.hub_sources)) {
        if (!(await save({ activeHubSource: entry.id }, true))) return;
      } else {
        await switchSource(entry.source);
      }
      toast(tf("settings.sourceSwitched", ui.lang, { name: hubSourceLabel(entry, settings) }), "success");
    } catch (err) {
      toast(tr("common.failed") + ": " + err.message, "error");
    } finally {
      hubSwitching = false;
    }
  }

  function resetDefaults() {
    if (!confirm("Reset all settings to defaults? This clears the saved app preferences and cannot be undone. It does not change server environment paths (such as HF_HUB_CACHE) or move any cached files.")) return;
    // Reset drops the hub list back to the two built-in sources and clears their
    // saved credentials: the baseline keeps the loaded mask, so the empty token is
    // authored instead of read as "unchanged".
    const storedTokens = new Map(hubSources(settings).map((entry) => [entry.id, entry.token]));
    hubs = hubDraft(hubSources({})).map((entry) => ({ ...entry, tokenBaseline: storedTokens.get(entry.id) || "" }));
    hubPick = hubs[0]?.id || "";
    tokenReset = true;
    transportReset = { connections: 8, maxActive: 3, maxSpeed: "", retries: 4, verify: "size" };
    storageReset = { cacheDir: "", localDir: "", localScanDirs: [], downloadRoutes: {} };
    form.proxyUrl = "";
    form.proxyUsername = "";
    form.proxyPassword = "";
    form.proxyNoProxy = "";
    form.proxyNoEnvProxy = false;
    toast("Settings reset to defaults — save to apply", "info");
  }

  // save posts the whole form. extra names the hub entry to make live, and silent
  // leaves the "saved" toast to a caller that reports the switch itself.
  async function save(extra = {}, silent = false) {
    if (ui.settingsLoadFailed) {
      toast("Settings are not loaded yet. Reload the page before saving.", "error");
      return false;
    }
    // The live triple has to describe the entry this request makes active, not
    // whichever tab happens to be open: a source/endpoint pair that belongs to a
    // different entry is the mismatch the server then rejects.
    const activeEntry =
      hubs.find((entry) => entry.id === (extra.activeHubSource || "")) ||
      hubs.find((entry) => isLiveHub(entry)) ||
      hubs[0];

    const body = {
      // Only Reset authors the transport and storage values. An ordinary save
      // leaves them alone so it cannot roll back a limit changed from the Jobs
      // page or a folder changed from the Cache page after this form loaded.
      ...(transportReset || {}),
      ...(storageReset || {}),
      endpoint: activeEntry?.endpoint || "",
      source: normalizeSource(activeEntry?.source || DEFAULT_SOURCE),
      // The list is authoritative: the server keeps each entry's stored token
      // unless this request authored a new one.
      hubSources: hubs.map(hubRequestBody),
      ...extra
    };

    // The loaded mask is display-only: only authored changes or Reset carry token
    // intent, so an ordinary Save preserves the stored credentials. The top-level
    // token describes the active entry, which is where the live credential lives.
    const activeAuthored = Boolean(activeEntry) && activeEntry.token !== activeEntry.tokenBaseline;
    if (tokenReset || activeAuthored) body.token = tokenReset ? "" : activeEntry.token;

    if (form.proxyUrl || form.proxyNoEnvProxy) {
      body.proxy = { url: form.proxyUrl, username: form.proxyUsername, noProxy: form.proxyNoProxy, noEnvProxy: form.proxyNoEnvProxy };
      if (form.proxyPassword) body.proxy.password = form.proxyPassword;
    }

    try {
      const result = await saveSettings(body);
      if (!silent) toast(result.message || tr("common.save"), "success");
      form.proxyPassword = "";
      transportReset = null;
      storageReset = null;
      // The saved settings are read back, so the tabs re-draft from the answer the
      // server actually stored, masks included.
      return true;
    } catch (err) {
      toast(err.message || tr("common.error"), "error");
      return false;
    }
  }
</script>

<header class="page-header">
  <div>
    <h1>{tr("nav.settings")}</h1>
    <p class="page-subtitle">{tr("settings.subtitle")}</p>
    <p class="page-config-path">
      {#if settings.configFile}
        <span class="config-chip" title={settings.configFile}>{tr("settings.configChipConfig")}: <code>{fileName(settings.configFile)}</code></span>
      {/if}
      {#if settings.targetsFile}
        <span class="config-chip" title={settings.targetsFile}>{tr("settings.configChipTargets")}: <code>{fileName(settings.targetsFile)}</code></span>
      {/if}
    </p>
  </div>
  <div class="page-actions">
    <button class="btn btn-ghost btn-sm" type="button" onclick={resetDefaults}>{tr("settings.reset")}</button>
    <button class="btn btn-primary" type="button" onclick={() => save()}>{tr("settings.save")}</button>
  </div>
</header>

<div class="settings-body">
  <div class="settings-layout">
    <div class="card settings-card settings-card-wide">
      <div class="card-header hub-card-header">
        <span class="card-icon"><Icon name="key" size={24} /></span>
        <h3>{tr("settings.hubAccess")}</h3>
        <button type="button" class="btn btn-ghost btn-sm hub-card-add" title={tr("settings.hubAddHint")} onclick={addHubSource}>
          <Icon name="plus" size={18} />
          <span>{tr("settings.hubAdd")}</span>
        </button>
      </div>
      <div class="card-body settings-rows">
        <div class="hub-tabs" role="tablist" aria-label={tr("settings.hubAccess")}>
          {#each hubs as entry (entry.id)}
            <span class="hub-tab-shell">
              <button type="button" role="tab" id={"hubTab-" + entry.id} class="hub-tab{entry.id === hubPick ? " is-active" : ""}" aria-selected={entry.id === hubPick} data-hub={entry.id} onclick={() => (hubPick = entry.id)}>
                <span class="hub-tab-name">{hubSourceLabel(entry, settings)}</span>
                {#if isLiveHub(entry)}<span class="hub-tab-badge">{tr("settings.hubCurrent")}</span>{/if}
              </button>
              <button type="button" class="hub-tab-remove" title={tr("settings.hubRemove")} aria-label={tr("settings.hubRemove") + " " + hubSourceLabel(entry, settings)} data-hub-remove={entry.id} onclick={() => removeHubSource(entry)}>
                <Icon name="close" size={14} />
              </button>
            </span>
          {/each}
        </div>
        {#if selectedHub}
          <div class="settings-rows hub-panel" role="tabpanel">
            <div class="settings-row">
              <label for="hubName" title={tr("settings.hubNameHint")}>{tr("settings.hubName")}</label>
              <div class="settings-control settings-control-md">
                <input type="text" id="hubName" placeholder={sourceName(selectedHub.source)} spellcheck="false" bind:value={selectedHub.name} />
              </div>
            </div>
            <div class="settings-row">
              <label for="hubKind" title={tr("settings.hubKindHint")}>{tr("settings.hubKind")}</label>
              <div class="settings-control settings-control-md">
                <select id="hubKind" bind:value={selectedHub.source}>
                  {#each SOURCE_ORDER as id}
                    <option value={id}>{SOURCE_NAMES[id]}</option>
                  {/each}
                </select>
              </div>
            </div>
            <div class="settings-row">
              <label for="endpoint" title={tr("settings.endpointHint")}>{tr("settings.endpoint")}</label>
              <div class="settings-control">
                <input type="text" id="endpoint" placeholder={hubDefaults[selectedHub.source] || ""} spellcheck="false" bind:value={selectedHub.endpoint} />
              </div>
            </div>
            <div class="settings-row">
              <label for="hfToken" title={tr("settings.tokenRequiredHint")}>{tr("settings.token")}</label>
              <div class="settings-control">
                <div class="input-with-toggle">
                  <input type={showToken ? "text" : "password"} id="hfToken" placeholder={selectedHub.source === "modelscope" ? "ModelScope access token" : "hf_xxxxxxxxxx"} aria-describedby="hfTokenHelp" bind:value={selectedHub.token} />
                  <button type="button" class="toggle-visibility" aria-label={tr("settings.showToken")} onclick={() => (showToken = !showToken)}>
                    <span class="icon-show"><Icon name="vision" size={20} /></span>
                    {#if showToken}<span class="icon-hide"><Icon name="vision" size={20} /></span>{/if}
                  </button>
                </div>
                <div class="form-hint" id="hfTokenHelp">{tr("settings.tokenHint")}</div>
              </div>
            </div>
            <div class="settings-row">
              <span class="settings-label-spacer"></span>
              <div class="settings-control settings-hub-actions">
                <button type="button" id="hubUse" class="btn btn-secondary btn-sm" disabled={hubSwitching || isLiveHub(selectedHub)} onclick={() => useActiveHub(selectedHub)}>
                  {#if hubSwitching}<span class="spinner-sm"></span>{/if}
                  <span>{tr("settings.hubUse")}</span>
                </button>
                <span class="form-hint">{tr("settings.hubUseHint")}</span>
              </div>
            </div>
          </div>
        {/if}
        <p class="form-hint hub-tabs-hint">{tr("settings.hubTabsHint")}</p>
      </div>
    </div>

    <div class="card settings-card settings-card-wide">
      <div class="card-header">
        <span class="card-icon"><Icon name="globe" size={24} /></span>
        <h3>{tr("settings.proxy")}</h3>
      </div>
      <div class="card-body settings-rows">
        <div class="settings-row">
          <label for="proxyUrl" title={tr("settings.proxyUrlHint")}>{tr("settings.proxyUrl")}</label>
          <div class="settings-control">
            <input type="text" id="proxyUrl" class="path-input" placeholder={tr("settings.proxyUrlPlaceholder")} spellcheck="false" bind:value={form.proxyUrl} />
          </div>
        </div>
        <div class="settings-row">
          <label>{tr("settings.credentials")}</label>
          <div class="settings-control settings-pair-control">
            <input type="text" id="proxyUsername" placeholder={tr("settings.proxyUserOptional")} spellcheck="false" bind:value={form.proxyUsername} />
            <div class="input-with-toggle">
              <input type={showProxyPassword ? "text" : "password"} id="proxyPassword" placeholder={tr("models.optional")} spellcheck="false" bind:value={form.proxyPassword} />
              <button type="button" class="toggle-visibility" aria-label={tr("settings.showPassword")} onclick={() => (showProxyPassword = !showProxyPassword)}>
                <span class="icon-show"><Icon name="vision" size={20} /></span>
                {#if showProxyPassword}<span class="icon-hide"><Icon name="vision" size={20} /></span>{/if}
              </button>
            </div>
          </div>
        </div>
        <div class="settings-row">
          <label for="proxyNoProxy" title={tr("settings.proxyNoProxyHint")}>{tr("settings.proxyNoProxy")}</label>
          <div class="settings-control">
            <input type="text" id="proxyNoProxy" class="path-input" placeholder={tr("settings.proxyNoProxyPlaceholder")} spellcheck="false" bind:value={form.proxyNoProxy} />
          </div>
        </div>
        <div class="settings-row">
          <span class="settings-label-spacer"></span>
          <div class="settings-control">
            <label class="checkbox-label">
              <input type="checkbox" id="proxyNoEnvProxy" bind:checked={form.proxyNoEnvProxy} />
              <span>{tr("settings.proxyNoEnv")}</span>
            </label>
          </div>
        </div>
      </div>
    </div>
  </div>
</div>
