import { test, expect } from "vitest";
import { renderMarkdown, safeReadmeHref } from "../src/markdown.js";

const BASE = "https://huggingface.co/owner/repo/raw/main/README.md";

test("keeps ordinary links and relative paths", () => {
  expect(safeReadmeHref("https://example.com/x", BASE)).toBe("https://example.com/x");
  expect(safeReadmeHref("./docs/x.png", BASE)).toBe("https://huggingface.co/owner/repo/raw/main/docs/x.png");
  expect(safeReadmeHref("docs/x.png", BASE)).toBe("https://huggingface.co/owner/repo/raw/main/docs/x.png");
});

test("refuses hrefs that would run code in the page", () => {
  // new URL() accepts these schemes, so they used to survive into the href and
  // Models.svelte injects this HTML with {@html}.
  expect(safeReadmeHref("javascript:alert(1)", BASE)).toBe("");
  expect(safeReadmeHref("vbscript:msgbox(1)", BASE)).toBe("");
  expect(safeReadmeHref("data:text/html,<script>alert(1)</script>", BASE)).toContain("data:");
});

test("renders a label without an anchor when the target is unsafe", () => {
  const html = renderMarkdown("[click](javascript:fetch('/api/jobs'))", BASE);
  expect(html).not.toContain("<a ");
  expect(html).toContain("click");
});

test("still renders safe links", () => {
  const html = renderMarkdown("[docs](https://example.com/guide)", BASE);
  expect(html).toContain('<a href="https://example.com/guide" target="_blank" rel="noopener">docs</a>');
});
