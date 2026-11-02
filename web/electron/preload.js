// Sandboxed preload scripts are loaded as CommonJS regardless of the
// "type":"module" setting in package.json — ESM `import` here fails with
// "Cannot use import statement outside a module", so use require().
const { contextBridge, ipcRenderer } = require("electron");

// Only window and notification capabilities cross the bridge. Data stays on the
// HTTP/WebSocket transport, so the renderer does not need Node APIs.
contextBridge.exposeInMainWorld("electronAPI", {
  notify: (message) => ipcRenderer.send("notify", String(message))
});
