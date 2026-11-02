import { test, expect } from "vitest";
import { wsConnect, isStaleBuild, buildSrcFromHtml } from "../src/api.js";

// wsConnect assigns handlers as properties, so a plain object is enough to test
// the seam without a real WebSocket.
function fakeSocket() {
  return { closed: false, close() { this.closed = true; } };
}

test("reports connection status and delivers messages", () => {
  const received = [];
  const socket = fakeSocket();
  const connection = wsConnect((message) => received.push(message), { factory: () => socket });

  socket.onopen();
  socket.onmessage({ data: JSON.stringify({ type: "job_update", data: { id: "a", status: "running" } }) });

  expect(received[0].type).toBe("status");
  expect(received[0].data.connected).toBe(true);
  expect(received[1].data.id).toBe("a");
  expect(connection.connection).toBe(socket);
});

test("malformed frames are ignored instead of throwing", () => {
  const received = [];
  const socket = fakeSocket();
  wsConnect((message) => received.push(message), { factory: () => socket });
  socket.onmessage({ data: "not json" });
  expect(received.length).toBe(0);
});

test("an explicit close does not reconnect", () => {
  const socket = fakeSocket();
  const connection = wsConnect(() => {}, { factory: () => socket, reconnectDelay: 1 });
  connection.close();
  expect(socket.closed).toBe(true);
});

test("onReconnect fires on a reconnect, not on the first open", async () => {
  let reconnects = 0;
  const first = fakeSocket();
  const second = fakeSocket();
  const sockets = [first, second];
  const connection = wsConnect(() => {}, { factory: () => sockets.shift(), reconnectDelay: 1, onReconnect: () => (reconnects += 1) });

  first.onopen();
  expect(reconnects).toBe(0);
  first.onclose();
  await new Promise((resolve) => setTimeout(resolve, 20));
  second.onopen();
  expect(reconnects).toBe(1);
  connection.close();
});

test("a page running a bundle the server no longer serves is stale", async () => {
  document.head.innerHTML = '<script type="module" src="/assets/index-old.js"></script>';
  const fetchImpl = async () => ({ ok: true, text: async () => '<script type="module" crossorigin src="/assets/index-new.js"></script>' });
  expect(await isStaleBuild({ fetchImpl, doc: document })).toBe(true);
});

test("a page running the served bundle is not stale", async () => {
  document.head.innerHTML = '<script type="module" src="/assets/index-same.js"></script>';
  const fetchImpl = async () => ({ ok: true, text: async () => '<script type="module" crossorigin src="/assets/index-same.js"></script>' });
  expect(await isStaleBuild({ fetchImpl, doc: document })).toBe(false);
});

test("a failed or unparseable probe never claims the page is stale", async () => {
  document.head.innerHTML = '<script type="module" src="/assets/index-same.js"></script>';
  expect(await isStaleBuild({ fetchImpl: async () => ({ ok: false, text: async () => "" }), doc: document })).toBe(false);
  expect(await isStaleBuild({ fetchImpl: async () => ({ ok: true, text: async () => "<html></html>" }), doc: document })).toBe(false);
  expect(buildSrcFromHtml("<script type=\"module\" crossorigin src=\"/assets/a.js\"></script>")).toBe("/assets/a.js");
});
