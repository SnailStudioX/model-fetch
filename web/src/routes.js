// Route labels are the only route-facing surface in the UI: keys are sent to the
// API but never rendered, matching the legacy behaviour.
export const ROUTE_LABELS = [
  { key: "llm/gguf", label: "GGUF models", preset: ["LLM", "GGUF"] },
  { key: "llm/safetensors", label: "Safetensors / LLM", preset: ["LLM", "Safetensors"] },
  { key: "audio", label: "Audio", preset: ["Audio"] },
  { key: "diffusion", label: "Diffusion", preset: ["Diffusion"] },
  { key: "embedding", label: "Embedding", preset: ["Embedding"] }
];

export function getConfiguredRouteOptions(settings) {
  const routes = (settings && settings.downloadRoutes) || {};
  return ROUTE_LABELS.filter((route) => String(routes[route.key] || "").trim() !== "" || (route.key.startsWith("llm/") && String(routes.llm || "").trim() !== ""))
    .map((route) => ({ key: route.key, label: route.label }));
}

export function routeKeyForAnalysis(analysis) {
  if (!analysis || analysis.is_dataset) return "";
  switch (analysis.type) {
    case "gguf":
      return "llm/gguf";
    case "transformers":
      return analysis.transformers && analysis.transformers.task === "feature-extraction" ? "embedding" : "llm/safetensors";
    case "gptq":
    case "awq":
      return "llm/safetensors";
    case "diffusers":
      return "diffusion";
    case "audio":
      return "audio";
    default:
      return "";
  }
}
