import { test, expect } from "vitest";
import { mount, tick, unmount } from "svelte";
import GpuCombobox from "../src/components/GpuCombobox.svelte";

const entries = [
  { name: "NVIDIA GeForce RTX 3090", vram_gb: 24, bandwidth_gbps: 936, cores: 10496 },
  { name: "rtx 4060 ti", vram_gb: 8, bandwidth_gbps: 288, cores: 4352 }
];

async function mountCombobox(onpick, value) {
  document.body.innerHTML = "";
  const component = mount(GpuCombobox, { target: document.body, props: { value, entries, lang: "en", onpick } });
  await tick();
  return component;
}

test("shows the short name and commits the short name", async () => {
  const picked = [];
  const component = await mountCombobox((name) => picked.push(name), "NVIDIA GeForce RTX 3090");
  const input = document.querySelector(".gpu-combobox-input");
  expect(input.value).toBe("RTX 3090");

  input.value = "4060";
  input.dispatchEvent(new Event("input", { bubbles: true }));
  await tick();

  const option = document.querySelector(".gpu-combobox-list button");
  const mousedown = new Event("mousedown", { bubbles: true, cancelable: true });
  option.dispatchEvent(mousedown);
  expect(mousedown.defaultPrevented).toBe(true);

  option.click();
  expect(picked).toEqual(["RTX 4060 Ti"]);
  unmount(component);
});

test("opening the list on a committed card still lists the other cards", async () => {
  const picked = [];
  const component = await mountCombobox((name) => picked.push(name), "NVIDIA GeForce RTX 3090");
  const input = document.querySelector(".gpu-combobox-input");

  // Focus without typing must not filter by the committed name, otherwise the
  // picked card is the only row and the pick can never be changed.
  input.dispatchEvent(new Event("focus"));
  await tick();

  const options = [...document.querySelectorAll(".gpu-combobox-list button")];
  expect(options.map((option) => option.querySelector(".gpu-combobox-name").textContent)).toEqual(["RTX 3090", "RTX 4060 Ti"]);

  options[1].click();
  expect(picked).toEqual(["RTX 4060 Ti"]);
  expect(document.querySelector(".gpu-combobox-source")).toBe(null);
  unmount(component);
});

test("typing then blurring commits once", async () => {
  const picked = [];
  const component = await mountCombobox((name) => picked.push(name), "NVIDIA GeForce RTX 3090");
  const input = document.querySelector(".gpu-combobox-input");

  input.value = "5090";
  input.dispatchEvent(new Event("input", { bubbles: true }));
  await tick();

  input.dispatchEvent(new Event("blur"));
  expect(picked).toEqual(["5090"]);

  input.dispatchEvent(new Event("blur"));
  expect(picked).toEqual(["5090"]);
  unmount(component);
});
