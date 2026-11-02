import { test, expect } from "vitest";
import { displayGpuName, formatDuration, formatRelativeDate, maxSpeedToMbps, mbpsToMaxSpeedStr } from "../src/format.js";
import { t } from "../src/i18n.js";

test("drops the vendor prefix and keeps the model token", () => {
  expect(displayGpuName("NVIDIA GeForce RTX 3090")).toBe("RTX 3090");
  expect(displayGpuName("AMD Radeon RX 580 2048SP")).toBe("RX 580 2048SP");
  expect(displayGpuName("Intel(R) UHD Graphics 630")).toBe("UHD Graphics 630");
  expect(displayGpuName("NVIDIA GeForce RTX 4060 Laptop GPU")).toBe("RTX 4060 Laptop GPU");
});

test("keeps a brand word when it is part of the model name", () => {
  expect(displayGpuName("Intel Arc A770")).toBe("Arc A770");
  expect(displayGpuName("AMD Radeon 780M")).toBe("Radeon 780M");
  expect(displayGpuName("arc a770m")).toBe("Arc A770M");
  expect(displayGpuName("rtx 4060 ti 16gb")).toBe("RTX 4060 Ti 16GB");
});

test("empty names stay empty", () => {
  expect(displayGpuName("")).toBe("");
  expect(displayGpuName("   ")).toBe("");
});

test("relative time and ETA follow the active language", () => {
  const tr = (key) => t(key, "zh");
  expect(formatRelativeDate(new Date(Date.now() - 30000).toISOString(), tr)).toBe("刚刚");
  expect(formatRelativeDate(new Date(Date.now() - 45 * 60000).toISOString(), tr)).toBe("45 分钟前");
  expect(formatRelativeDate(new Date(Date.now() - 3 * 3600000).toISOString(), tr)).toBe("3 小时前");
  expect(formatRelativeDate(new Date(Date.now() - 4 * 86400000).toISOString(), tr)).toBe("4 天前");
  expect(formatDuration(0, tr)).toBe("预计 --");
  expect(formatDuration(3720, tr)).toBe("预计 1 小时 2 分");
  expect(formatDuration(45, tr)).toBe("预计 45 秒");
  expect(formatRelativeDate(new Date(Date.now() - 30000).toISOString(), (key) => t(key, "en"))).toBe("just now");
  expect(formatDuration(3720, (key) => t(key, "en"))).toBe("ETA 1h 2m");
});

test("the speed cap is posted in the grammar the settings API accepts", () => {
  // "52428800 B/s" is a 400 Invalid maxSpeed; the cap has to travel as a byte size.
  expect(mbpsToMaxSpeedStr(5)).toBe("5120KiB");
  expect(mbpsToMaxSpeedStr(15)).toBe("15360KiB");
  expect(mbpsToMaxSpeedStr(25)).toBe("25600KiB");
  expect(mbpsToMaxSpeedStr(50)).toBe("51200KiB");
  expect(mbpsToMaxSpeedStr(0.5)).toBe("512KiB");
  expect(mbpsToMaxSpeedStr(0)).toBe("");
  expect(mbpsToMaxSpeedStr("")).toBe("");
  expect(mbpsToMaxSpeedStr("abc")).toBe("");
  // A positive cap must never round down to "0KiB", which the server reads as unlimited.
  expect(mbpsToMaxSpeedStr(0.001)).toBe("1.02KiB");
  expect(mbpsToMaxSpeedStr(0.0001)).toBe("1KiB");
});

test("the stored byte rate reads back as the megabyte the control shows", () => {
  // The gauge on the Jobs page is 1024-based, so the cap has to divide the same way.
  expect(maxSpeedToMbps("5120KiB")).toBeCloseTo(5, 6);
  expect(maxSpeedToMbps("1MiB")).toBeCloseTo(1, 6);
  expect(maxSpeedToMbps("5120KiB/s")).toBeCloseTo(5, 6);
  expect(maxSpeedToMbps("0")).toBe(0);
  expect(maxSpeedToMbps("")).toBe(0);
  expect(maxSpeedToMbps("abc")).toBe(0);
  for (const mbps of [5, 15, 25, 50, 100]) {
    expect(maxSpeedToMbps(mbpsToMaxSpeedStr(mbps))).toBeCloseTo(mbps, 6);
  }
});
