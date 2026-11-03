// Takes the README screenshots with the running app (headless Electron window).
// Usage: npm run shots -- [--lang=zh|en] [serverURL]
// Expects the server on http://127.0.0.1:8081 (override: last argument).
// Output: ../../docs/screenshots/modelfetch-<page>.png, or modelfetch-<page>-<lang>.png
// for a language the readme pair does not boot in by default (the English one).

import { app, BrowserWindow } from "electron";
import { writeFileSync, mkdirSync } from "node:fs";
import { fileURLToPath } from "node:url";
import path from "node:path";

// Offscreen capture with GPU compositing leaves half-finished transition frames
// (observed: nav highlight smears), so render on the CPU.
app.disableHardwareAcceleration();

const here = path.dirname(fileURLToPath(import.meta.url));
const outDir = path.resolve(here, "../../docs/screenshots");

// Where the caller's arguments land in argv depends on how Electron was
// launched, so match them by shape rather than by position.
const langFlag = process.argv.find((arg) => arg.startsWith("--lang="));
const lang = langFlag ? langFlag.slice("--lang=".length) : "zh";
if (!["zh", "en"].includes(lang)) {
  console.error(`capture-shots: unknown --lang=${JSON.stringify(lang)} (expected zh or en)`);
  app.exit(1);
}
const base = process.argv.find((arg) => /^https?:\/\//.test(arg)) || "http://127.0.0.1:8081";
// The Chinese set is what the app boots in, so it keeps the unsuffixed names.
const suffix = lang === "zh" ? "" : `-${lang}`;

// One entry per README screenshot: the nav page to open, and what "rendered"
// looks like there. Models has a fallback path because the recommend feed's
// upstream is flaky: if no rows appear, type a keyword search instead.
const shots = [
  { page: "models", ready: ".model-row", timeout: 12000, images: ".model-row-avatar-img" },
  { page: "jobs", ready: ".speed-bar", timeout: 5000 },
  { page: "cache", ready: ".cache-card, .cache-table-row, .empty-state", timeout: 10000 },
  { page: "mirror", ready: ".sync-control-panel", timeout: 5000 },
  { page: "settings", ready: ".settings-card", timeout: 5000 },
];

const wait = (ms) => new Promise((r) => setTimeout(r, ms));

// Polls a selector inside the page. Returns true once it resolves to a node.
async function waitFor(win, selector, timeout) {
  const deadline = Date.now() + timeout;
  while (Date.now() < deadline) {
    const found = await win.webContents.executeJavaScript(
      `!!document.querySelector(${JSON.stringify(selector)})`
    );
    if (found) return true;
    await wait(300);
  }
  return false;
}

// Avatar logos are lazy-loaded images that paint over their initials fallback,
// so a row list can look "ready" while every logo is still in flight. A failed
// image also reaches complete=true (it is removed by onerror), so waiting here
// cannot stall on a broken host.
async function waitForImages(win, selector, timeout) {
  const deadline = Date.now() + timeout;
  while (Date.now() < deadline) {
    const done = await win.webContents.executeJavaScript(`(() => {
      const imgs = [...document.querySelectorAll(${JSON.stringify(selector)})];
      return imgs.length > 0 && imgs.every((i) => i.complete);
    })()`);
    if (done) return true;
    await wait(300);
  }
  return false;
}

// The app veils the page while slow requests are in flight (follow-up reads
// after a search can re-trigger it), so a capture taken during one shows the
// spinner card over blurred content. Wait for the veil to be gone.
async function waitForNoLoading(win, timeout) {
  const deadline = Date.now() + timeout;
  while (Date.now() < deadline) {
    const idle = await win.webContents.executeJavaScript("!document.querySelector('.global-loading')");
    if (idle) return true;
    await wait(250);
  }
  return false;
}

// The models list depends on the hub's search upstream, which intermittently
// 502s through the mirror. If the default recommend feed returns nothing, type
// keyword searches until rows appear, cycling keywords and retrying each.
async function ensureModelRows(win) {
  if (await waitFor(win, ".model-row", 12000)) return true;
  const keywords = ["deepseek", "qwen", "llama"];
  for (let attempt = 0; attempt < 3; attempt++) {
    for (const kw of keywords) {
      await win.webContents.executeJavaScript(`(() => {
        const i = document.querySelector('.models-search-input');
        if (!i) return false;
        i.focus();
        i.value = '';
        i.dispatchEvent(new Event('input', { bubbles: true }));
        i.value = ${JSON.stringify(kw)};
        i.dispatchEvent(new Event('input', { bubbles: true }));
        i.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
        return true;
      })()`);
      if (await waitFor(win, ".model-row", 15000)) return true;
      await wait(2000);
    }
  }
  return false;
}

async function main() {
  await app.whenReady();
  mkdirSync(outDir, { recursive: true });

  const win = new BrowserWindow({
    width: 1600,
    height: 1000,
    show: false,
    backgroundColor: "#ededed",
    webPreferences: { sandbox: false },
  });

  await win.loadURL(base + "/");
  if (!(await waitFor(win, ".nav-item", 15000))) {
    console.error("app shell never rendered — is the server running at " + base + "?");
    app.exit(1);
  }
  // The language is a persisted choice the app reads at startup
  // (web/src/store.svelte.js), so set it and reload: every page below then
  // renders in it from its first paint, as it would for a reader of that readme.
  await win.webContents.executeJavaScript(
    `localStorage.setItem("modelfetch.lang", ${JSON.stringify(lang)}); true`
  );
  await win.loadURL(base + "/");
  if (!(await waitFor(win, ".nav-item", 15000))) {
    console.error(`app shell never rendered in ${lang}`);
    app.exit(1);
  }
  // Let the default page's data and fonts settle before the first capture.
  await wait(1500);

  for (const shot of shots) {
    await win.webContents.executeJavaScript(
      `document.querySelector('button.nav-item[data-page=${JSON.stringify(shot.page)}]')?.click(); true`
    );
    let ready = await waitFor(win, shot.ready, shot.timeout);
    if (!ready && shot.page === "models") ready = await ensureModelRows(win);
    if (!ready) console.warn(`warning: ${shot.page} never reached its ready state; capturing anyway`);
    if (shot.images) await waitForImages(win, shot.images, 12000);
    await waitForNoLoading(win, 15000);
    // Let nav transitions and lazy images finish painting before the capture.
    await wait(2500);
    const image = await win.webContents.capturePage();
    const file = path.join(outDir, `modelfetch-${shot.page}${suffix}.png`);
    writeFileSync(file, image.toPNG());
    console.log("wrote", file);
  }

  app.exit(0);
}

main().catch((err) => {
  console.error(err);
  app.exit(1);
});
