import { test, expect, beforeEach } from "vitest";
import { ui, dlStatusFor, setDlStatus } from "../src/store.svelte.js";

beforeEach(() => {
  ui.jobs.clear();
  ui.models.dlStatus = {};
});

test("reading download status does not mutate the store it reads", () => {
  setDlStatus("owner/model", "q4_k_m", "running");
  setDlStatus("owner/model", "q6_k", "done");

  const map = dlStatusFor("owner/model");

  // The stale running entry is ignored for display but must stay in the store:
  // dlStatusFor is called from a $derived, so writing here would be an unsafe
  // mutation of the state the derived reads.
  expect(map).toEqual({ q6_k: "done" });
  expect(ui.models.dlStatus["owner/model"]).toEqual({ q4_k_m: "running", q6_k: "done" });
  expect(map).not.toBe(ui.models.dlStatus["owner/model"]);
});

test("live jobs still drive the displayed status, including upstream companions", () => {
  ui.jobs.set("job-1", { id: "job-1", repo: "upstream/repo", localRepo: "owner/model", filters: ["q4_k_m"], status: "queued" });

  expect(dlStatusFor("owner/model")).toEqual({ q4_k_m: "queued" });
  expect(dlStatusFor("upstream/repo")).toEqual({});
});
