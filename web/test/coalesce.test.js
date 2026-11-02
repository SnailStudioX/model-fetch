import { test, expect } from "vitest";
import { createCoalescer } from "../src/coalesce.js";

test("coalesces multiple updates for the same job into one flush", () => {
  const target = new Map();
  const frames = [];
  const coalescer = createCoalescer(target, (fn) => frames.push(fn));

  coalescer.apply({ id: "a", status: "running", downloadedBytes: 10 });
  coalescer.apply({ id: "a", status: "running", downloadedBytes: 20 });
  coalescer.apply({ id: "b", status: "queued" });

  expect(frames.length).toBe(1);
  expect(target.size).toBe(0);

  frames[0]();
  expect(target.get("a").downloadedBytes).toBe(20);
  expect(target.get("b").status).toBe("queued");
  expect(coalescer.pending.size).toBe(0);
});

test("re-arms after a flush", () => {
  const target = new Map();
  const frames = [];
  const coalescer = createCoalescer(target, (fn) => frames.push(fn));
  coalescer.apply({ id: "a" });
  frames[0]();
  coalescer.apply({ id: "b" });
  expect(frames.length).toBe(2);
});

test("ignores malformed updates", () => {
  const target = new Map();
  const coalescer = createCoalescer(target, () => {});
  coalescer.apply(null);
  coalescer.apply({ status: "running" });
  expect(coalescer.pending.size).toBe(0);
});
