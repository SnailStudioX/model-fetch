import { test, expect } from "vitest";
import { readdirSync, readFileSync, statSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { t } from "../src/i18n.js";

function resolveSourceRoot() {
  try {
    return fileURLToPath(new URL("../src/", import.meta.url));
  } catch {
    return path.resolve("src");
  }
}

const sourceRoot = resolveSourceRoot();

function sourceFiles(dir) {
  const out = [];
  for (const entry of readdirSync(dir)) {
    const full = path.join(dir, entry);
    if (statSync(full).isDirectory()) out.push(...sourceFiles(full));
    else if (entry.endsWith(".js") || entry.endsWith(".svelte")) out.push(full);
  }
  return out;
}

function usedKeys() {
  const keys = new Set();
  for (const file of sourceFiles(sourceRoot)) {
    const text = readFileSync(file, "utf8");
    for (const match of text.matchAll(/\btr\("([a-z0-9._]+)"|\btf\("([a-z0-9._]+)"|\bt\("([a-z0-9._]+)"/g)) {
      const key = match[1] || match[2] || match[3];
      if (key.endsWith(".")) continue; // dynamic key prefix, e.g. tr("models.type." + key)
      keys.add(key);
    }
  }
  return keys;
}

test("every key the UI asks for exists in both catalogs", () => {
  const missing = [...usedKeys()].filter((key) => t(key, "zh") === key || t(key, "en") === key);
  expect(missing).toEqual([]);
});

test("catalogs stay aligned for the labels that were hardcoded English", () => {
  const spotChecks = [
    ["cache.detailTitle", "仓库详情", "Repository Details"],
    ["cache.detailPaths", "路径", "Paths"],
    ["cache.pathCache", "缓存（HF 格式）", "Cache (HF format)"],
    ["cache.pathFriendly", "友好视图", "Friendly view"],
    ["models.type.text-to-image", "文生图", "Text to image"],
    ["mirror.sectionTargetsHint", "缓存要同步到的位置（NAS、U 盘、办公室服务器）", "Destinations for syncing your cache (NAS, USB, office server)"],
    ["jobs.status.running", "下载中", "Running"],
    ["capability.vision", "视觉", "Vision"],
    ["mirror.toastPushed", "已推送 {n} 个仓库（{size}）", "Pushed {n} repos ({size})"],
    ["models.recBadge", "推荐", "rec"],
  ];
  for (const [key, zh, en] of spotChecks) {
    expect(t(key, "zh")).toBe(zh);
    expect(t(key, "en")).toBe(en);
  }
});

test("dynamic key families resolve for the values the UI can render", () => {
  for (const type of ["text-generation", "text-to-image", "image-to-text", "automatic-speech-recognition", "text-to-speech", "sentence-similarity"]) {
    expect(t("models.type." + type, "zh")).not.toBe("models.type." + type);
  }
  for (const status of ["running", "paused", "queued", "failed", "cancelled", "completed", "finalizing", "active", "pending"]) {
    expect(t("jobs.status." + status, "zh")).not.toBe("jobs.status." + status);
  }
  for (const kind of ["vision", "tools", "reasoning"]) {
    expect(t("capability.title." + kind, "zh")).not.toBe("capability.title." + kind);
  }
  // The cache-check panel builds its labels from whatever the server reported, so
  // an unhandled state or finding kind would render its key on screen.
  for (const state of ["running", "completed", "failed", "cancelled"]) {
    expect(t("cache.check." + state, "zh")).not.toBe("cache.check." + state);
    expect(t("cache.check." + state, "en")).not.toBe("cache.check." + state);
  }
  for (const kind of ["no-blobs", "empty-blob", "hash-mismatch", "unreadable", "unexpected-entry", "dangling-snapshot"]) {
    expect(t("cache.check.kind." + kind, "zh")).not.toBe("cache.check.kind." + kind);
    expect(t("cache.check.kind." + kind, "en")).not.toBe("cache.check.kind." + kind);
  }
});
