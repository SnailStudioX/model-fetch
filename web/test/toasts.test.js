// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

import { test, expect, beforeEach, afterEach, vi } from "vitest";
import { mount, tick, unmount } from "svelte";
import Toasts from "../src/components/Toasts.svelte";
import { ui, toast, dismissToast } from "../src/store.svelte.js";

// The reported bug: a toast stayed on screen forever, so the corner filled up
// with old confirmations. The store now owns the lifetime of a toast.

let component = null;

beforeEach(() => {
  vi.useFakeTimers();
  document.body.innerHTML = "";
  ui.toasts = [];
  component = mount(Toasts, { target: document.body });
});

afterEach(() => {
  unmount(component);
  component = null;
  document.body.innerHTML = "";
  ui.toasts = [];
  vi.useRealTimers();
});

test("a toast slides out and is removed without any user action", async () => {
  toast("Deleted owner/repo from cache", "success");
  await tick();
  expect(document.querySelectorAll(".toast").length).toBe(1);

  vi.advanceTimersByTime(4100);
  await tick();
  const leaving = document.querySelectorAll(".toast.toast-leaving");
  expect(ui.toasts.length).toBe(1);
  expect(leaving.length).toBe(1);

  vi.advanceTimersByTime(400);
  await tick();
  expect(ui.toasts.length).toBe(0);
  expect(document.querySelectorAll(".toast").length).toBe(0);
});

test("ids stay unique after a toast is removed", async () => {
  toast("first", "info");
  vi.advanceTimersByTime(4500);
  await tick();
  toast("second", "success");
  await tick();

  const ids = ui.toasts.map((item) => item.id);
  expect(ids.length).toBe(1);
  expect(ui.toasts[0].message).toBe("second");
  expect(document.querySelectorAll(".toast").length).toBe(1);
});

test("dismissToast is idempotent while the exit animation is running", async () => {
  const id = toast("only one", "info");
  await tick();
  dismissToast(id);
  dismissToast(id);
  await tick();
  expect(ui.toasts.length).toBe(1);
  expect(ui.toasts[0].leaving).toBe(true);

  vi.advanceTimersByTime(400);
  await tick();
  expect(ui.toasts.length).toBe(0);
});

test("the stack is capped instead of growing forever", async () => {
  for (let i = 0; i < 9; i += 1) toast("toast " + i, "info");
  await tick();
  const live = ui.toasts.filter((item) => !item.leaving);
  expect(live.length).toBeLessThanOrEqual(5);

  vi.advanceTimersByTime(4500);
  await tick();
  expect(ui.toasts.length).toBe(0);
});
