// Runs the Electron shell (or another electron-launched script) with a clean
// environment: some toolchains leak ELECTRON_RUN_AS_NODE=1 into their child
// envs, and electron.exe then behaves as a bare Node interpreter — the main
// process dies with confusing "electron does not export BrowserWindow" style
// errors. Stripping it here makes npm run app reproducible from any host.
// Usage: node scripts/run-app.mjs [electron-args...]   (default arg: ".")

import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";
import path from "node:path";

// In plain Node, the electron package default-exports the binary path.
const { default: electronPath } = await import("electron");

const args = process.argv.slice(2);
if (args.length === 0) args.push(".");

const env = { ...process.env };
if (env.ELECTRON_RUN_AS_NODE) {
  console.error("run-app: clearing inherited ELECTRON_RUN_AS_NODE so electron.exe runs as Electron, not bare Node");
  delete env.ELECTRON_RUN_AS_NODE;
}

const webDir = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const child = spawn(electronPath, args, { stdio: "inherit", cwd: webDir, env });
child.on("exit", (code, signal) => process.exit(signal ? 1 : (code ?? 1)));
