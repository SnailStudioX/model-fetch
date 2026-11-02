// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

// Hub source helpers shared by the Models header selector and the Settings form.
//
// A source and an endpoint are one decision, not two: the server rejects a
// source whose API shape does not fit the endpoint host, so switching the source
// has to send the endpoint that belongs to it. Sending the source alone is what
// the user sees as "the switch does nothing".

export const SOURCE_NAMES = { modelscope: "ModelScope", hf: "Hugging Face" };

// Display order. ModelScope comes first because it is the default source and the
// hub reachable from the networks this project is usually used on.
export const SOURCE_ORDER = ["modelscope", "hf"];

// Default source for a fresh install, and for any settings response that does
// not name one (an older server, or the first paint before settings load).
export const DEFAULT_SOURCE = "modelscope";

const SOURCE_ALIASES = { huggingface: "hf", ms: "modelscope" };

// normalizeSource maps the aliases the server accepts onto the ids the UI lists,
// so a stored "ms" never renders as an unknown option.
export function normalizeSource(id) {
  const raw = String(id || "").trim().toLowerCase();
  const resolved = SOURCE_ALIASES[raw] || raw;
  return SOURCE_NAMES[resolved] ? resolved : DEFAULT_SOURCE;
}

export function activeSource(settings) {
  return normalizeSource(settings?.source);
}

export function sourceName(id) {
  return SOURCE_NAMES[normalizeSource(id)];
}

// sourceDefaults merges the endpoints the server advertises over the built-in
// ones, so the UI prefills the host the server itself would use. The Hugging Face
// fallback is the mirror, not huggingface.co, because that is the endpoint an empty
// setting resolves to on the server too.
export function sourceDefaults(settings) {
  return {
    modelscope: "https://modelscope.cn",
    hf: DEFAULT_HF_ENDPOINT,
    ...(settings?.source_defaults || {})
  };
}

function hostOf(value) {
  const raw = String(value || "").trim();
  if (!raw) return "";
  try {
    return new URL(raw.includes("://") ? raw : "https://" + raw).host.toLowerCase();
  } catch {
    return raw.toLowerCase().replace(/\/.*$/, "");
  }
}

// Built-in mirror of the server's host ownership table (source_hosts in
// GET /api/settings), used only before that response arrives.
const SOURCE_HOSTS = {
  hf: ["huggingface.co", "hf.co", "hf-mirror.com"],
  modelscope: ["modelscope.cn"]
};

// sourceHosts merges the server's table over the built-in one. A host a source
// claims answers that source's API shape, which is what makes an endpoint
// attributable to a switch rather than to the user.
export function sourceHosts(settings) {
  const merged = {};
  for (const [id, hosts] of Object.entries(SOURCE_HOSTS)) merged[id] = hosts.map(hostOf);
  for (const [id, hosts] of Object.entries(settings?.source_hosts || {})) {
    if (!id || !Array.isArray(hosts)) continue;
    const key = String(id).toLowerCase();
    merged[key] = [...new Set([...(merged[key] || []), ...hosts.map(hostOf)])];
  }
  return merged;
}

// switchSourceBody builds the settings body for a source switch.
//
// The endpoint is replaced when it is empty or sits on a host that another source
// claims, because that value was written by an earlier switch: hf-mirror.com
// answers the Hugging Face paths and never ModelScope's, so keeping it across a
// switch is the mismatch the server then rejects and the user sees as a selector
// that does nothing. A host no source claims (a private mirror, a self-hosted
// instance) is kept, because only the user can know which hub it serves.
export function switchSourceBody(settings, next, endpointValue, remembered = {}) {
  const defaults = sourceDefaults(settings);
  const target = normalizeSource(next);
  const typed = String(endpointValue === undefined ? (settings?.endpoint || "") : endpointValue).trim();

  const foreign = new Set();
  for (const [id, hosts] of Object.entries(sourceHosts(settings))) {
    if (normalizeSource(id) === target) continue;
    for (const host of hosts) if (host) foreign.add(host);
  }
  for (const [id, url] of Object.entries(defaults)) {
    if (normalizeSource(id) === target) continue;
    const host = hostOf(url);
    if (host) foreign.add(host);
  }

  const keep = typed !== "" && !foreign.has(hostOf(typed));

  // When the endpoint does have to be replaced, prefer the one this source was last
  // used with over the built-in default: a user who downloads through hf-mirror.com
  // and takes a trip to ModelScope expects to land back on their own mirror.
  const rememberedEndpoint = String(remembered[target] || "").trim();
  const rememberedHost = hostOf(rememberedEndpoint);
  const usable = rememberedEndpoint !== "" && (!rememberedHost || !foreign.has(rememberedHost));
  const fallback = (usable ? rememberedEndpoint : "") || defaults[target] || "";

  return { source: target, endpoint: keep ? typed : fallback };
}
// Default endpoint for a Hugging Face tab. huggingface.co is unreachable from the
// networks this project is usually used on, while hf-mirror.com answers the same
// API shape, so a fresh install points its Hugging Face source at the mirror.
export const DEFAULT_HF_ENDPOINT = "https://hf-mirror.com";

// hubSources returns the saved hub access sources: the Settings tabs and the
// Models page selector, in the order the server saved them.
//
// The server owns the list (hub-sources in modelfetch.json) and reports every
// credential masked. A server that predates the list gets the two sources the UI
// has always offered, with the live pair folded into the entry it belongs to, so
// both controls keep working against an older server.
export function hubSources(settings) {
  const saved = Array.isArray(settings?.hub_sources) ? settings.hub_sources : [];
  const activeId = String(settings?.active_hub_source || "").trim();
  if (saved.length) {
    const list = [];
    for (const entry of saved) {
      const source = normalizeSource(entry?.source);
      const id = String(entry?.id || "").trim() || source;
      if (list.some((item) => item.id === id)) continue;
      list.push({
        id,
        name: String(entry?.name || "").trim() || sourceName(source),
        source,
        endpoint: String(entry?.endpoint || "").trim(),
        token: String(entry?.token || ""),
        active: id === activeId
      });
    }
    if (list.length) {
      // A list whose active id the UI cannot see still has to show one current tab,
      // or the selector would render with nothing selected.
      if (!list.some((item) => item.active)) list[0].active = true;
      return list;
    }
  }
  const live = activeSource(settings);
  const list = [
    { id: "modelscope", name: SOURCE_NAMES.modelscope, source: "modelscope", endpoint: "", token: "", active: false },
    { id: "hf", name: SOURCE_NAMES.hf, source: "hf", endpoint: DEFAULT_HF_ENDPOINT, token: "", active: false }
  ];
  for (const entry of list) {
    if (entry.source !== live) continue;
    entry.active = true;
    entry.endpoint = String(settings?.endpoint || "").trim() || entry.endpoint;
    entry.token = String(settings?.token || "");
  }
  return list;
}

// activeHubId is the entry the server is really using, which is what the Models
// selector has to show on first paint.
export function activeHubId(settings) {
  const list = hubSources(settings);
  const wanted = String(settings?.active_hub_source || "").trim();
  return (list.find((entry) => entry.active && entry.id === wanted) || list.find((entry) => entry.active) || list[0])?.id || "modelscope";
}

// hubSourceLabel is the tab and option text: the saved name, plus the host when
// the entry points somewhere other than its source's own default, because two
// Hugging Face tabs that download from different mirrors have to be tellable apart.
export function hubSourceLabel(entry, settings) {
  if (!entry) return "";
  const name = String(entry.name || "").trim() || sourceName(entry.source);
  const endpoint = String(entry.endpoint || "").trim();
  if (!endpoint) return name;
  const defaults = sourceDefaults(settings);
  const host = hostOf(endpoint);
  if (!host || host === hostOf(defaults[normalizeSource(entry.source)])) return name;
  return name + " · " + host;
}

// newHubBody builds the settings body that makes one saved hub source the live
// one. Naming the entry is enough: the server keeps the live source, endpoint and
// token equal to the active entry, so the credential travels with the hub instead
// of following the user across it.
export function newHubBody(id) {
  return { activeHubSource: String(id || "").trim() };
}