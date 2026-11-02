import { defineConfig } from "vite";
import { svelte } from "@sveltejs/vite-plugin-svelte";

// Build output is embedded by internal/assets/webui, so the shipped binary stays
// a single file. Keep outDir inside the package that embeds it.
export default defineConfig({
  plugins: [svelte({ compilerOptions: { runes: true } })],
  // The outDir is outside this package, so Vite would otherwise leave the previous
  // hashed bundles on disk; they are embedded too, so each build would add another
  // ~200 KB copy of the UI to the binary.
  build: { outDir: "../internal/assets/webui/dist", emptyOutDir: true },
  // Component tests need the client runtime, not the SSR entry.
  resolve: { conditions: ["browser"] },
  // jsdom was being created once per test file (21 times) and dominated the run.
  // vmThreads creates the jsdom environment once per worker thread while still
  // giving every file its own VM module context, so per-file isolation - the
  // tests lean on it, e.g. models-search.test.js captures ui.models.gguf at
  // import time - stays intact. isolate:false would share the module registry
  // across files and make that capture order-dependent; it passed here only by
  // scheduling luck and is not safe to rely on.
  test: { environment: "jsdom", pool: "vmThreads" }
});
