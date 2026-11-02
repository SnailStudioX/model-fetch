// Markdown subset matching the legacy renderer: fenced code, headings, bullets,
// inline code/bold/links, images and paragraphs. Kept deliberately small so the
// output stays identical to the server-rendered README the API already provides.
export function escapeHtml(value) {
  return String(value).replace(/[&<>"']/g, (character) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[character]));
}

export function resolveReadmeURL(url, baseRawURL) {
  const clean = String(url || "").trim();
  if (/^(https?:|data:)/i.test(clean)) return clean;
  try {
    return new URL(clean.replace(/^\.\//, ""), baseRawURL).toString();
  } catch {
    return clean;
  }
}

const SAFE_README_PROTOCOLS = new Set(["http:", "https:", "data:"]);

// safeReadmeHref returns a usable href, or "" when the target would run code in
// the page. resolveReadmeURL keeps anything new URL() accepts, and javascript:
// and vbscript: are valid schemes, so a README link written as
// [click](javascript:fetch(...)) would otherwise be injected as a clickable
// anchor. The server-rendered README is sanitized with bluemonday; this is the
// fallback renderer Models.svelte uses when the API returned markdown only.
export function safeReadmeHref(url, baseRawURL) {
  const resolved = resolveReadmeURL(url, baseRawURL);
  if (!resolved) return "";
  try {
    return SAFE_README_PROTOCOLS.has(new URL(resolved, baseRawURL).protocol) ? resolved : "";
  } catch {
    return "";
  }
}

export function renderMarkdownInline(text, baseRawURL) {
  let html = escapeHtml(text);
  html = html.replace(/`([^`]+)`/g, "<code>$1</code>");
  html = html.replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>");
  html = html.replace(/\[([^\]]+)\]\(([^)]+)\)/g, (match, label, href) => {
    const url = safeReadmeHref(href, baseRawURL);
    if (!url) return label;
    return '<a href="' + escapeHtml(url) + '" target="_blank" rel="noopener">' + label + "</a>";
  });
  return html;
}

export function renderMarkdown(source, baseRawURL) {
  const lines = String(source || "").split("\n");
  const html = [];
  let paragraph = [];
  let inCode = false;

  const flushParagraph = () => {
    if (paragraph.length) {
      html.push("<p>" + renderMarkdownInline(paragraph.join(" "), baseRawURL) + "</p>");
      paragraph = [];
    }
  };

  for (const rawLine of lines.slice(0, 220)) {
    const line = rawLine.trimEnd();
    if (line.startsWith("```")) {
      flushParagraph();
      inCode = !inCode;
      if (inCode) html.push("<pre><code>");
      else html.push("</code></pre>");
      continue;
    }
    if (inCode) {
      html.push(escapeHtml(line) + "\n");
      continue;
    }
    if (!line.trim()) {
      flushParagraph();
      continue;
    }

    const imageMatch = line.match(/^!\[([^\]]*)\]\(([^)]+)\)/);
    if (imageMatch) {
      flushParagraph();
      html.push('<img src="' + escapeHtml(resolveReadmeURL(imageMatch[2], baseRawURL)) + '" alt="' + escapeHtml(imageMatch[1]) + '" loading="lazy">');
      continue;
    }

    const heading = line.match(/^(#{1,3})\s+(.+)$/);
    if (heading) {
      flushParagraph();
      const level = Math.min(heading[1].length + 3, 6);
      html.push("<h" + level + ">" + renderMarkdownInline(heading[2], baseRawURL) + "</h" + level + ">");
      continue;
    }

    if (/^[-*]\s+/.test(line)) {
      flushParagraph();
      html.push('<p class="readme-bullet">' + renderMarkdownInline(line.replace(/^[-*]\s+/, ""), baseRawURL) + "</p>");
      continue;
    }

    paragraph.push(line);
  }

  flushParagraph();
  if (inCode) html.push("</code></pre>");
  return html.join("");
}
