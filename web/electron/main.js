import { app, BrowserWindow, Notification, Tray, dialog, ipcMain, session } from "electron";
import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";
import fs from "node:fs";
import path from "node:path";

let child = null;
let window = null;
let tray = null;

// The Go server ships as a sidecar, not inside this process: the HTTP/WebSocket
// transport and the API contract stay identical to browser and Docker mode.
function resolveBinary() {
  // An explicit override always wins (custom installs, CI smoke runs).
  if (process.env.MODELFETCH_BINARY) return process.env.MODELFETCH_BINARY;
  const binName = process.platform === "win32" ? "modelfetch.exe" : "modelfetch";
  if (app.isPackaged) {
    // electron-builder's extraResources places the server directly under
    // <resources>/; it is not inside the asar because it must be executable.
    return path.join(process.resourcesPath, binName);
  }
  // Development: the binary `go build -o modelfetch[.exe] ./cmd/modelfetch`
  // leaves at the repository root, then a PATH lookup as the last resort.
  const repoBinary = fileURLToPath(new URL("../../" + binName, import.meta.url));
  if (fs.existsSync(repoBinary)) return repoBinary;
  return binName;
}

if (!app.requestSingleInstanceLock()) {
  app.quit();
} else {
  app.on("second-instance", () => {
    if (window) {
      if (window.isMinimized()) window.restore();
      window.focus();
    }
  });

  async function startServer() {
    // Port 0 lets the OS pick a free port, so a busy 8080 or a second instance
    // never fails. The server prints the resolved URL once it is listening.
    child = spawn(resolveBinary(), ["--bind", "127.0.0.1", "--port", "0", "--no-open", "--print-url"], {
      stdio: ["ignore", "pipe", "pipe"]
    });

    child.stderr.on("data", (chunk) => process.stderr.write(chunk));

    return new Promise((resolve, reject) => {
      let buffer = "";
      child.stdout.on("data", (chunk) => {
        buffer += chunk;
        const match = buffer.match(/MODELFETCH_URL (\S+)/);
        if (match) resolve(match[1]);
      });
      child.on("error", reject);
      child.on("exit", (code) => reject(new Error("server exited with code " + code)));
      setTimeout(() => reject(new Error("server did not report its URL within 10s")), 10000);
    });
  }

  app.whenReady().then(async () => {
    // Electron exposes no session-level CSP hook, and the CSP it injects into
    // packaged apps omits ws:, which would block the loopback WebSocket. Tag
    // the server's document response instead. connect-src 'self' on an http
    // origin does not cover the ws scheme, so name the loopback host (no port
    // in the source expression = any port, which --port 0 makes necessary).
    session.defaultSession.webRequest.onHeadersReceived({ urls: ["*://127.0.0.1/*"] }, (details, callback) => {
      const headers = details.responseHeaders;
      if (headers && details.resourceType === "mainFrame") {
        headers["Content-Security-Policy"] = [
          "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self' ws://127.0.0.1; img-src 'self' data: https:"
        ];
      }
      callback({ responseHeaders: headers });
    });

    let url;
    try {
      url = await startServer();
    } catch (err) {
      dialog.showErrorBox(
        "ModelFetch",
        "Could not start the local server:\n" + err.message +
        "\n\nSet MODELFETCH_BINARY to point at a modelfetch binary."
      );
      app.quit();
      return;
    }

    // .pathname keeps a leading slash before a Windows drive letter, so every
    // fileURLToPath here has to decode file: URLs properly instead.
    const preload = fileURLToPath(new URL("./preload.js", import.meta.url));
    const trayIcon = fileURLToPath(new URL("../build/tray.png", import.meta.url));

    window = new BrowserWindow({
      width: 1200,
      height: 800,
      icon: fileURLToPath(new URL("../build/icon.png", import.meta.url)),
      webPreferences: { preload, contextIsolation: true, nodeIntegration: false }
    });
    await window.loadURL(url);
    console.log("desktop shell: server at " + url);

    // A missing icon or an unsupported tray (bare X sessions) must not take
    // the window down with it.
    if (fs.existsSync(trayIcon)) {
      try {
        tray = new Tray(trayIcon);
        tray.setToolTip("ModelFetch");
        tray.on("click", () => window?.show());
      } catch (err) {
        console.error("tray:", err.message);
      }
    } else {
      console.error("tray icon missing at " + trayIcon);
    }

    ipcMain.on("notify", (_event, message) => {
      new Notification({ title: "ModelFetch", body: String(message) }).show();
    });
  });

  app.on("before-quit", () => {
    // Stop the sidecar explicitly; an orphaned server keeps the port open.
    if (child && !child.killed) child.kill("SIGTERM");
  });

  app.on("window-all-closed", () => {
    if (process.platform !== "darwin") app.quit();
  });
}
