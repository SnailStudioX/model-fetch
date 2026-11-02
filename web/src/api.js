// Transport seam. The UI talks to the Go server only through these two
// functions, so a native IPC transport can replace them without touching any
// component. Base is empty when the page is served by the same server.
import { requestStarted, requestFinished } from "./loading.svelte.js";

const BASE = (import.meta.env?.VITE_API_BASE || "").replace(/\/$/, "");

export async function apiFetch(path, options = {}) {
  // Every apiFetch feeds the global loading tally, so the indicator covers
  // slow responses page by page cannot predict. fetch() itself only rejects
  // on network failure; the finally still runs for HTTP errors thrown below.
  requestStarted();
  try {
    const res = await fetch(BASE + path, {
      headers: { "Content-Type": "application/json" },
      ...options
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) {
      const detail = data.error_detail || { code: "http_error", message: data.error || `HTTP ${res.status}` };
      const err = new Error(detail.message || "Request failed");
      err.code = detail.code;
      err.details = detail.details;
      err.status = res.status;
      throw err;
    }
    return data;
  } finally {
    requestFinished();
  }
}

export function wsConnect(onMessage, { reconnectDelay = 2000, factory, onReconnect } = {}) {
  let socket = null;
  let closed = false;
  let timer = null;
  let everOpened = false;

  function open() {
    socket = factory ? factory() : new WebSocket(`${location.protocol === "https:" ? "wss:" : "ws:"}//${location.host}/api/ws`);
    socket.onmessage = (event) => {
      let message = null;
      try {
        message = JSON.parse(event.data);
      } catch {
        return;
      }
      if (message && message.type) onMessage(message);
    };
    socket.onopen = () => {
      onMessage({ type: "status", data: { connected: true } });
      // A reconnect means the server underneath this page may have been replaced, which
      // is exactly when the page can be running a bundle the server no longer ships.
      if (everOpened) onReconnect?.();
      everOpened = true;
    };
    socket.onclose = () => {
      onMessage({ type: "status", data: { connected: false } });
      if (closed) return;
      timer = setTimeout(open, reconnectDelay);
    };
    return socket;
  }

  const connection = open();
  return {
    connection,
    close() {
      closed = true;
      if (timer) clearTimeout(timer);
      connection.close();
    }
  };
}

// The served index.html names the bundle it wants the browser to run. A tab left
// open across a server restart keeps executing the bundle it loaded at load time,
// so it goes on showing behaviour that no longer exists in the binary answering
// its requests. Comparing the two bundle names is how the page notices.
export function buildSrcFromHtml(html) {
  const match = /<script[^>]*\ssrc="([^"]+\.js)"[^>]*>/i.exec(html || "");
  return match ? match[1] : "";
}

function resolveSrc(src, base) {
  try {
    return new URL(src, base).href;
  } catch {
    return String(src);
  }
}

export async function isStaleBuild({ fetchImpl = globalThis.fetch, doc = document } = {}) {
  const current = doc.querySelector("script[src]")?.getAttribute("src") || "";
  if (!current || typeof fetchImpl !== "function") return false;
  const res = await fetchImpl("./", { cache: "no-store" });
  if (!res || !res.ok) return false;
  const served = buildSrcFromHtml(await res.text());
  if (!served) return false;
  const base = doc.baseURI || "http://localhost/";
  return resolveSrc(served, base) !== resolveSrc(current, base);
}
