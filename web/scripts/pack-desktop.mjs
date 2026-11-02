// Packages the desktop client end to end: builds the Svelte UI (embedded by the
// Go binary), builds the Go server as the sidecar electron will ship, then runs
// electron-builder, whose extraResources copy that sidecar into the app bundle.
// Usage:
//   npm run pack            -> installers in web/release/
//   npm run pack -- --dir   -> unpacked app directory only (fast config check)
// Extra args are passed through to electron-builder (e.g. -l for Linux targets).

import { spawnSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import path from "node:path";

const here = path.dirname(fileURLToPath(import.meta.url));
const webDir = path.resolve(here, "..");
const repoDir = path.resolve(webDir, "..");
const binName = process.platform === "win32" ? "modelfetch.exe" : "modelfetch";
const version = readFileSync(path.join(repoDir, "VERSION"), "utf8").trim();
if (!version) {
  console.error("VERSION file is empty; the installer needs a version");
  process.exit(1);
}

function run(cmd, args, opts = {}) {
  console.log(`> ${cmd} ${args.join(" ")}`);
  const res = spawnSync(cmd, args, { stdio: "inherit", ...opts });
  if (res.error) {
    console.error(String(res.error.message));
    process.exit(1);
  }
  if (res.status !== 0) process.exit(res.status ?? 1);
}

// Order matters: vite writes internal/assets/webui/dist, and only then does
// go build embed it — a stale sidecar would ship a stale UI silently.
// Go through `npm run build`, not the vite binary: vite empties dist (wiping
// the tracked .placeholder) and only the postbuild hook puts it back.
// shell:true because recent Node refuses to spawn npm.cmd directly (EINVAL).
run("npm", ["run", "build"], { cwd: webDir, shell: process.platform === "win32" });

// Static sidecar, same recipe as build.sh: CGO off, version injected.
run("go", ["build", "-trimpath", `-ldflags=-X main.Version=${version}`, "-o", path.join("web", "build", "sidecar", binName), "./cmd/modelfetch"], {
  cwd: repoDir,
  env: { ...process.env, CGO_ENABLED: "0" }
});

// The UI package stays versionless; the installer inherits the app version.
run(process.execPath, [
  path.join(webDir, "node_modules", "electron-builder", "cli.js"),
  `--config.extraMetadata.version=${version}`,
  ...process.argv.slice(2)
], { cwd: webDir });
