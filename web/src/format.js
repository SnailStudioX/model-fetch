export function formatBytes(bytes) {
  if (!bytes || bytes === 0) return "0 B";
  const k = 1024;
  const sizes = ["B", "KB", "MB", "GB", "TB"];
  const i = Math.floor(Math.log(bytes) / Math.log(k));
  return parseFloat((bytes / Math.pow(k, i)).toFixed(1)) + " " + sizes[i];
}

export function formatNumber(n) {
  if (!n) return "0";
  if (n >= 1000000) return (n / 1000000).toFixed(1) + "M";
  if (n >= 1000) return (n / 1000).toFixed(1) + "K";
  return String(n);
}

export function formatRelativeDate(iso, translate = (value) => value) {
  try {
    const d = new Date(iso);
    const diff = Date.now() - d.getTime();
    const mins = Math.floor(diff / 60000);
    if (mins < 1) return translate("format.justNow");
    if (mins < 60) return translate("format.minutesAgo").replace("{n}", mins);
    const hrs = Math.floor(mins / 60);
    if (hrs < 24) return translate("format.hoursAgo").replace("{n}", hrs);
    const days = Math.floor(hrs / 24);
    if (days < 7) return translate("format.daysAgo").replace("{n}", days);
    return d.toLocaleDateString();
  } catch {
    return iso;
  }
}

export function formatDuration(seconds, translate = (value) => value) {
  const unit = (key, n) => translate(key).replace("{n}", n);
  if (!Number.isFinite(seconds) || seconds <= 0) return translate("format.etaUnknown");
  const s = Math.max(1, Math.round(seconds));
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  const eta = (value) => translate("format.eta").replace("{t}", value);
  if (h > 0) return eta([unit("format.hours", h), unit("format.minutes", m)].join(" "));
  if (m >= 2) return eta(unit("format.minutes", m));
  const sec = Math.max(5, Math.round(s / 5) * 5);
  if (sec >= 60) {
    const mm = Math.floor(sec / 60);
    const ss = sec % 60;
    return eta(ss > 0 ? [unit("format.minutes", mm), unit("format.seconds", ss)].join(" ") : unit("format.minutes", mm));
  }
  return eta(unit("format.seconds", sec));
}

export function statusCategory(status) {
  if (status === "running") return "running";
  if (status === "queued") return "queued";
  if (status === "paused") return "paused";
  if (status === "completed" || status === "failed" || status === "cancelled") return "done";
  return status || "unknown";
}

// The speed controls speak megabytes per second (MB/s, 1024-based) because that is the
// unit the job cards already use for live speed; the backend stores a byte-rate string.
// The server parser (pkg/hfdownloader ParseSizeStrict) only accepts "<number><unit>" with
// B/KB/MB/GB/TB (decimal) or KiB/MiB/GiB/TiB (binary). A rate spelling such as
// "52428800 B/s" is rejected with 400 Invalid maxSpeed, which is what made the speed limit
// impossible to change: the cap never moved and the control snapped back to unlimited.
// KiB is used on the wire so the cap matches the 1024-based gauge: 1 MB/s = 1024 KiB.
const SIZE_UNITS = { B: 1, KB: 1e3, MB: 1e6, GB: 1e9, TB: 1e12, KIB: 1024, MIB: 1048576, GIB: 1073741824, TIB: 1099511627776 };

export function mbpsToMaxSpeedStr(mbps) {
  const value = Number(mbps);
  if (!Number.isFinite(value) || value <= 0) return "";
  // KiB keeps the presets exact (5 MB/s -> 5120KiB) and the stored value readable. It is
  // floored at 1KiB so a positive cap can never round down to "0KiB", which the server
  // reads as unlimited and would silently drop the cap just set.
  const kib = Math.round(value * 1024 * 100) / 100;
  if (kib < 1) return "1KiB";
  return (Number.isInteger(kib) ? String(kib) : kib.toFixed(2)) + "KiB";
}

export function maxSpeedToMbps(value) {
  if (!value) return 0;
  // Accept what the server stores ("5120KiB", "2MB", "0") and the per-second spelling a
  // hand-edited config file may use ("5120KiB/s").
  const match = String(value).trim().match(/^([\d.]+)\s*([KMGT]?i?B)?(\/s)?$/i);
  if (!match) return 0;
  const number = Number(match[1]);
  if (!Number.isFinite(number)) return 0;
  const bytes = number * (SIZE_UNITS[(match[2] || "B").toUpperCase()] || 1);
  return bytes / 1048576;
}
// Disk chip severity mirrors the legacy thresholds so both frontends read the
// same risk level from the same free-space number.
export function diskSeverity(free) {
  if (!Number.isFinite(free) || free < 500 * 1024 * 1024) return "disk-free-danger";
  if (free < 2 * 1024 * 1024 * 1024) return "disk-free-warn";
  return "disk-free-ok";
}

export function gradeClass(grade) {
  return grade === "?" ? "unknown" : grade;
}

export function fitTitle(fit, translate = (value) => value) {
  const parts = [];
  if (fit.params_billions) parts.push(translate("format.paramsBillions").replace("{n}", fit.params_billions));
  if (fit.needed_gb) parts.push(translate("format.neededGb").replace("{n}", fit.needed_gb));
  if (fit.est_tokens_per_sec) parts.push(translate("format.tokPerSec").replace("{n}", fit.est_tokens_per_sec));
  if (fit.reason) parts.push(translate(fit.reason));
  parts.push(translate("format.nameBasedEstimate"));
  return parts.join(" · ");
}

// Only the conventional "<friendly root>/hub" is implied by the friendly cache
// folder, so a Hub path equal to it adds no information to the history row.
export function meaningfulHubDir(hubDir, outputDir) {
  const hub = String(hubDir || "").trim();
  if (!hub) return false;
  const base = String(outputDir || "").trim().replace(/[\\/]+$/, "");
  if (!base) return true;
  const conventional = (base + "/hub").replace(/\\/g, "/");
  return hub.replace(/\\/g, "/").replace(/\/+$/, "") !== conventional;
}

export function getTargetIcon(name) {
  const lower = String(name || "").toLowerCase();
  if (lower.includes("nas") || lower.includes("network")) return "network";
  if (lower.includes("usb") || lower.includes("drive") || lower.includes("external")) return "drive";
  if (lower.includes("office") || lower.includes("work") || lower.includes("server")) return "server";
  if (lower.includes("cloud") || lower.includes("remote")) return "cloud";
  return "mirror";
}

export function parseHumanBytes(value) {
  if (!value) return 0;
  const match = String(value).trim().match(/^([\d.]+)\s*([KMGT]?i?B)?$/i);
  if (!match) return 0;
  const number = parseFloat(match[1]);
  if (!Number.isFinite(number)) return 0;
  const unit = (match[2] || "B").toUpperCase();
  const factors = { B: 1, KB: 1e3, MB: 1e6, GB: 1e9, TB: 1e12, KIB: 1024, MIB: 1048576, GIB: 1073741824, TIB: 1099511627776 };
  return number * (factors[unit] || 1);
}
// Detected adapter names carry the vendor prefix ("NVIDIA GeForce RTX 3090"),
// while the catalog keys are the short model names. Display uses the short form
// so a slot box and its catalog row read the same way.
const gpuBrandCase = {
  geforce: "GeForce", radeon: "Radeon", quadro: "Quadro", tesla: "Tesla", arc: "Arc",
  vega: "Vega", iris: "Iris", firepro: "FirePro", adreno: "Adreno", mali: "Mali",
  dgx: "DGX", pro: "Pro", ti: "Ti", xt: "XT"
};

export function displayGpuName(name) {
  const clean = String(name || "").trim();
  if (!clean) return "";
  const short = clean
    .replace(/\(r\)/gi, " ")
    .replace(/^(nvidia|amd|intel|apple|arm|qualcomm)\s+/i, "")
    .replace(/^(geforce|radeon)\s+(?=(rtx|gtx|rx)\b)/i, "");
  return short.split(/\s+/).filter(Boolean).map((token) => {
    const lower = token.toLowerCase();
    if (gpuBrandCase[lower]) return gpuBrandCase[lower];
    if (/^(rtx|gtx|rx|uhd|dgx|gh\d+|gb\d+|b\d+|h\d+|a\d+\w*|l\d+\w*|t\d+\w*|\d+\w*)$/.test(lower)) return token.toUpperCase();
    return token;
  }).join(" ");
}
