import { test, expect } from "vitest";
import { apiFetch } from "../src/api.js";

test("maps the structured error payload onto the thrown error", async () => {
  globalThis.fetch = async () => ({
    ok: false,
    status: 400,
    json: async () => ({ ok: false, error: "Invalid request body", error_detail: { code: "bad_request", message: "Invalid request body", details: "unknown route key" } })
  });

  const err = await apiFetch("/api/download").catch((error) => error);
  expect(err.code).toBe("bad_request");
  expect(err.details).toBe("unknown route key");
  expect(err.status).toBe(400);
});

test("returns parsed JSON on success", async () => {
  globalThis.fetch = async () => ({ ok: true, status: 200, json: async () => ({ targets: [{ name: "nas", path: "/mnt/nas", exists: true }] }) });
  const result = await apiFetch("/api/mirror/targets");
  expect(result.targets[0].exists).toBe(true);
});
