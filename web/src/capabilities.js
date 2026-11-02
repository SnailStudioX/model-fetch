// Capability signals come from the server when available; the name/tag heuristics
// mirror the legacy UI so badges stay comparable between the two frontends.
export const capabilityLabels = { vision: "Vision", tools: "Tools", reasoning: "Reasoning" };

export function detectSearchCapabilities(result) {
  const tags = (result.tags || []).map((tag) => String(tag).toLowerCase());
  const haystack = [result.id, result.pipelineTag, result.libraryName, ...tags].filter(Boolean).join(" ").toLowerCase();
  const capabilities = [];
  const titles = {};
  const hasAny = (needles) => needles.some((needle) => haystack.includes(needle));
  const hasTag = (needles) => needles.some((needle) => tags.includes(needle));
  const pipeline = String(result.pipelineTag || "").toLowerCase();

  if (
    ["image-to-text", "image-text-to-text", "visual-question-answering", "image-classification", "object-detection"].includes(pipeline) ||
    hasTag(["vision", "computer-vision", "multimodal", "vlm"]) ||
    hasAny([" vision", "vision-", "-vision", "vlm", "multimodal", "mmproj", "image-to-text", "image-text-to-text"])
  ) {
    capabilities.push("vision");
    titles.vision = "capability.title.vision";
  }

  if (hasTag(["tool", "tools", "tool-use", "function-calling"]) || hasAny(["tool-use", "tool_use", "function-calling", "function_calling", "tools", " tool "])) {
    capabilities.push("tools");
    titles.tools = "capability.title.tools";
  }

  if (hasTag(["reasoning", "reasoner", "thinking"]) || hasAny(["reasoning", "reasoner", "thinking", "deepseek-r1", "qwq", "cot", "chain-of-thought"])) {
    capabilities.push("reasoning");
    titles.reasoning = "capability.title.reasoning";
  }

  return { capabilities, titles };
}

export function mergeCapabilities(serverCapabilities, detected) {
  return Array.from(new Set([...(serverCapabilities || []), ...detected]));
}
