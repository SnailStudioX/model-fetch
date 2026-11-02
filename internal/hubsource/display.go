// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hubsource

import "strings"

// displayNames are the human-readable labels for the registered source ids.
// A saved hub access source is shown to the user by name, so the label lives
// next to the registry instead of being duplicated by every client.
var displayNames = map[string]string{
	"hf":         "Hugging Face",
	"modelscope": "ModelScope",
}

// DisplayName returns the label for a source id. Accepted aliases resolve to the
// canonical name ("ms" -> "ModelScope"). An id no source claims is returned as
// written, so a hand-edited config still shows something readable instead of
// silently being relabelled "Hugging Face".
func DisplayName(id string) string {
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		return displayNames["hf"]
	}
	lower := strings.ToLower(trimmed)
	if name, ok := displayNames[lower]; ok {
		return name
	}
	if !Known(lower) {
		return trimmed
	}
	if name, ok := displayNames[Resolve(lower, "").ID()]; ok {
		return name
	}
	return trimmed
}
