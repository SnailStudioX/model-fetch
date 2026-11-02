// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package smartdl

import (
	"math"
	"sort"
	"strconv"
	"strings"
)

// Hardware describes the machine a model is recommended for. Every field is
// optional: a zero value means "unknown", and the caller must not guess.
// Bandwidth is never measured — it is a catalog lookup (see gpuCatalog),
// because no OS API exposes memory bus width.
type Hardware struct {
	GPUName      string  `json:"gpu_name,omitempty"`
	VRAMGB       float64 `json:"vram_gb,omitempty"`
	SystemRAMGB  float64 `json:"system_ram_gb,omitempty"`
	BandwidthGBs float64 `json:"bandwidth_gbps,omitempty"`
	GPUCount     int     `json:"gpu_count,omitempty"`
	Unified      bool    `json:"unified_memory,omitempty"`
	ContextLen   int     `json:"context_length,omitempty"`
	AllowOffload bool    `json:"allow_offload,omitempty"`

	// Source records where each number came from so the UI can say it:
	// "detected", "manual", "catalog", or "" when nothing is known.
	Source string `json:"source,omitempty"`
}

// FitStatus is the hardware fit classification for one downloadable item.
const (
	FitUnknown    = "unknown"
	FitCanRun     = "can-run"
	FitTight      = "tight"
	FitCanRunSlow = "can-run-slow" // fits only by offloading part of the model to system RAM
	FitCannotRun  = "cannot-run"
)

// HardwareFit is the per-item hardware assessment. It is additive: existing
// SelectableItem fields keep their meaning, so clients that ignore it are
// unaffected.
type HardwareFit struct {
	Fit                  string  `json:"fit"`
	Grade                string  `json:"grade"`
	Score                int     `json:"score"`
	MemoryPct            float64 `json:"memory_pct,omitempty"`
	EstTokensPerSec      int     `json:"est_tokens_per_sec,omitempty"`
	NeededGB             float64 `json:"needed_gb,omitempty"`
	KVBytesPerToken      int64   `json:"kv_bytes_per_token,omitempty"`
	KVSource             string  `json:"kv_source,omitempty"`              // "config" or "flat"
	ActiveParamsBillions float64 `json:"active_params_billions,omitempty"` // MoE per-token params
	ParamsBillions       float64 `json:"params_billions,omitempty"`
	Reason               string  `json:"reason,omitempty"`
}

// ModelShape carries the architecture facts the fit model needs: the KV-cache
// geometry and the parameter split. A zero Layers or KVHeads means unknown, and
// the caller falls back to FlatOverheadGB.
type ModelShape struct {
	Layers  int `json:"layers,omitempty"`
	KVHeads int `json:"kv_heads,omitempty"`
	HeadDim int `json:"head_dim,omitempty"`

	// KVBytesPerToken is computed from the fields above when they are known.
	KVBytesPerToken int64 `json:"kv_bytes_per_token,omitempty"`

	// ParamsBillions is the total parameter count. ActiveParamsBillions is the
	// per-token subset for a Mixture-of-Experts model; it is 0 for dense models.
	// All expert weights still have to be resident, so only the speed estimate
	// uses the active figure, never the fit.
	ParamsBillions       float64 `json:"params_billions,omitempty"`
	ActiveParamsBillions float64 `json:"active_params_billions,omitempty"`
}

// Constants behind the fit/score model. These follow the widely used
// hardware-fit convention (memory-bound decode, headroom, quality bonus) and
// are kept here as named values so tests and the UI can state them.
const (
	// FlatOverheadGB is the runtime + KV allowance used when the KV shape is
	// unknown. It is deliberately small: it underestimates long context.
	FlatOverheadGB = 0.5

	// SystemRAMBandwidthGBs is the practical dual-channel DDR figure used for
	// the offload path. Measured single-thread copy bandwidth is far lower and
	// is not a usable roofline input.
	SystemRAMBandwidthGBs = 50.0

	// Decode efficiency factors (roofline, memory-bound generation).
	efficiencyDiscrete = 0.70
	efficiencyUnified  = 0.65

	// Fit thresholds.
	discreteCanRunFactor = 0.85
	discreteTightFactor  = 1.10
	unifiedUsableFactor  = 0.75
	unifiedCanRunFactor  = 0.70
	offloadRAMFactor     = 0.70

	// Score weights.
	scoreWeightSpeed    = 0.55
	scoreWeightHeadroom = 0.35
	qualityBonusCap     = 12.0

	// Score penalties by fit.
	fitMultiplierSlow  = 0.60
	fitMultiplierTight = 0.75

	// Practical ceiling for the roofline estimate on one consumer GPU.
	rooflineCapTokPerSec = 100
)

// ComputeKVBytesPerToken returns the KV cache cost for one token:
// 2 * layers * kv_heads * head_dim * dtype_size, with fp16 KV assumed.
// Returns 0 when the shape is incomplete.
func (s ModelShape) ComputeKVBytesPerToken() int64 {
	if s.Layers <= 0 || s.KVHeads <= 0 || s.HeadDim <= 0 {
		return 0
	}
	return int64(2) * int64(s.Layers) * int64(s.KVHeads) * int64(s.HeadDim) * 2
}

// gpuCatalog maps a GPU name to (VRAM GB, bandwidth GB/s). Bandwidth cannot be
// read from any OS API, so it is a spec lookup. Names are matched
// case-insensitively against the substring the detector reports.
type catalogEntry struct {
	vramGB float64
	bwGBs  float64
	cores  int
}

// LookupBandwidth returns the catalog bandwidth for a GPU name, or 0 when the
// card is not in the catalog. The caller must not invent a value.
func LookupBandwidth(name string) float64 {
	e, ok := lookupCatalog(name)
	if !ok {
		return 0
	}
	return e.bwGBs
}

// LookupVRAM returns the catalog VRAM for a GPU name, or 0 when unknown.
func LookupVRAM(name string) float64 {
	e, ok := lookupCatalog(name)
	if !ok {
		return 0
	}
	return e.vramGB
}

// LookupCores returns the catalog shader/CUDA core count for a GPU name, or 0
// when the card is unknown or the source carries no core figure. Like bandwidth,
// this is a spec sheet value, not a measurement.
func LookupCores(name string) int {
	e, ok := lookupCatalog(name)
	if !ok {
		return 0
	}
	return e.cores
}

// lookupCatalog resolves a device name to its catalog row. An exact key wins;
// otherwise a row matches when every word of its key appears as a word of the
// device name, and the row whose key has the most words wins.
//
// Words, not raw substrings: adapter strings put the memory size behind a
// separator that the catalog key writes as a space, so "NVIDIA A100-SXM4-40GB"
// used to match only the bare "a100" key and report 80 GiB for a 40 GiB card.
// Longest-key-wins is what keeps "RTX 3080 12GB" ahead of "RTX 3080"; map
// iteration order is otherwise random.
func lookupCatalog(name string) (catalogEntry, bool) {
	key := normalizeGPUName(name)
	if key == "" {
		return catalogEntry{}, false
	}
	if e, ok := gpuCatalog[key]; ok {
		return e, true
	}
	want := gpuTokens(name)
	if len(want) == 0 {
		return catalogEntry{}, false
	}
	best, bestKey, bestWords := catalogEntry{}, "", 0
	for k, e := range gpuCatalog {
		words := gpuTokens(k)
		if len(words) == 0 || !wordsAllIn(words, want) {
			continue
		}
		if len(words) > bestWords || (len(words) == bestWords && len(k) > len(bestKey)) {
			best, bestKey, bestWords = e, k, len(words)
		}
	}
	if bestKey == "" {
		return catalogEntry{}, false
	}
	return best, true
}

// gpuTokens splits a device name into the words that identify it. "-", "_" and
// "." are word boundaries, which is what lets the catalog key "a100 40gb" line
// up with the adapter string "A100-SXM4-40GB".
func gpuTokens(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
	})
}

// wordsAllIn reports whether every word of sub appears as a word of hay.
func wordsAllIn(sub, hay []string) bool {
	for _, want := range sub {
		found := false
		for _, have := range hay {
			if have == want {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// CatalogNames returns the catalog GPU names in sorted order, for the settings
// autocomplete list.
func CatalogNames() []string {
	names := make([]string, 0, len(gpuCatalog))
	for name := range gpuCatalog {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// CatalogEntry is one catalog row, exposed so the UI can resolve a picked device
// without a second round trip.
type CatalogEntry struct {
	VRAMGB       float64 `json:"vram_gb"`
	BandwidthGBs float64 `json:"bandwidth_gbps"`
	Cores        int     `json:"cores"`
}

// CatalogEntries returns the catalog as name -> values. Keys match CatalogNames.
func CatalogEntries() map[string]CatalogEntry {
	out := make(map[string]CatalogEntry, len(gpuCatalog))
	for name, e := range gpuCatalog {
		out[name] = CatalogEntry{VRAMGB: e.vramGB, BandwidthGBs: e.bwGBs, Cores: e.cores}
	}
	return out
}

// LookupCatalogEntry resolves a GPU name to its catalog row, using the same
// longest-substring rule as the recommendation path.
func LookupCatalogEntry(name string) (CatalogEntry, bool) {
	e, ok := lookupCatalog(name)
	if !ok {
		return CatalogEntry{}, false
	}
	return CatalogEntry{VRAMGB: e.vramGB, BandwidthGBs: e.bwGBs, Cores: e.cores}, true
}

// SameDeviceName reports whether two spellings name the same card, so a Settings
// override that repeats the detected adapter is not treated as another machine.
// "RTX 3090" and "NVIDIA GeForce RTX 3090" are the same card; "RTX 3080" and
// "RTX 3080 Ti" are not.
//
// The catalog is the arbiter: two names that resolve to the same row are the
// same card, and an override that resolves to a different row is a different
// machine even though one string contains the other. A substring test alone said
// they were the same, which silently discarded the user's VRAM override for the
// Ti. When a name resolves nowhere, that test is still the fallback.
func SameDeviceName(a, b string) bool {
	na, nb := normalizeGPUName(a), normalizeGPUName(b)
	if na == "" || nb == "" {
		return false
	}
	if ea, ok := lookupCatalog(na); ok {
		if eb, ok := lookupCatalog(nb); ok {
			return ea == eb
		}
	}
	if na == nb {
		return true
	}
	return indexOfNormalized(na, nb) >= 0 || indexOfNormalized(nb, na) >= 0
}

func normalizeGPUName(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r == ' ' {
			out = append(out, ' ')
			continue
		}
		if r >= 'A' && r <= 'Z' {
			out = append(out, rune(r+32))
			continue
		}
		out = append(out, r)
	}
	return trimSpaces(string(out))
}

func indexOfNormalized(hay, needle string) int {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

func trimSpaces(s string) string {
	for len(s) > 0 && s[0] == ' ' {
		s = s[1:]
	}
	for len(s) > 0 && s[len(s)-1] == ' ' {
		s = s[:len(s)-1]
	}
	return s
}

// FitForHardware classifies one item against the hardware. neededGB is the
// weight bytes plus the KV allowance for the configured context.
func FitForHardware(neededGB float64, hw Hardware) string {
	if hw.VRAMGB <= 0 && hw.SystemRAMGB <= 0 {
		return FitUnknown
	}
	if hw.Unified || hw.VRAMGB <= 0 {
		usable := hw.SystemRAMGB * unifiedUsableFactor
		if usable <= 0 {
			return FitUnknown
		}
		if neededGB <= usable*unifiedCanRunFactor {
			return FitCanRun
		}
		if neededGB <= usable {
			return FitTight
		}
		return FitCannotRun
	}
	if neededGB <= hw.VRAMGB*discreteCanRunFactor {
		return FitCanRun
	}
	if neededGB <= hw.VRAMGB*discreteTightFactor {
		return FitTight
	}
	if hw.AllowOffload && hw.SystemRAMGB > hw.VRAMGB {
		total := hw.VRAMGB + hw.SystemRAMGB*offloadRAMFactor
		if neededGB <= total {
			return FitCanRunSlow
		}
	}
	return FitCannotRun
}

// memoryPct is the share of the usable memory the item needs. It can exceed
// 100 when the model only fits by offloading.
func memoryPct(neededGB float64, hw Hardware) float64 {
	base := hw.VRAMGB
	if hw.Unified || hw.VRAMGB <= 0 {
		base = hw.SystemRAMGB * unifiedUsableFactor
	}
	if base <= 0 {
		return 0
	}
	return neededGB / base * 100
}

// EstimateTokensPerSec is the roofline decode estimate. When the model does not
// fully reside in VRAM, the effective bandwidth is the harmonic mean of the two
// paths, which is bottlenecked by system RAM. The result is capped: the roofline
// is optimistic for small active parameter sets (MoE), where compute and routing
// overhead, not bandwidth, become the limit.
func EstimateTokensPerSec(workingSetGB float64, hw Hardware, residentGB float64) int {
	if workingSetGB <= 0 || hw.BandwidthGBs <= 0 {
		return 0
	}
	eff := efficiencyDiscrete
	if hw.Unified || hw.VRAMGB <= 0 {
		eff = efficiencyUnified
	}
	if residentGB > hw.VRAMGB && hw.VRAMGB > 0 && hw.SystemRAMGB > 0 {
		fracVRAM := hw.VRAMGB / residentGB
		if fracVRAM > 1 {
			fracVRAM = 1
		}
		fracRAM := 1 - fracVRAM
		if fracRAM <= 0 {
			return clampTokPerSec(int(math.Round(hw.BandwidthGBs / workingSetGB * eff)))
		}
		effBW := 1 / (fracVRAM/hw.BandwidthGBs + fracRAM/SystemRAMBandwidthGBs)
		// Extra penalty for the PCIe transfer of the resident part.
		return clampTokPerSec(int(math.Max(1, math.Round(effBW/workingSetGB*eff*0.85))))
	}
	return clampTokPerSec(int(math.Round(hw.BandwidthGBs / workingSetGB * eff)))
}

func clampTokPerSec(n int) int {
	if n > rooflineCapTokPerSec {
		return rooflineCapTokPerSec
	}
	return n
}

// score combines speed, memory headroom and a quality bonus.
func score(fit string, toksPerSec int, paramsBillions, memPct float64) int {
	if fit == FitCannotRun || fit == FitUnknown {
		return 0
	}
	var speedScore float64
	switch {
	case toksPerSec >= 80:
		speedScore = 100
	case toksPerSec >= 40:
		speedScore = lerp(float64(toksPerSec), 40, 80, 80, 100)
	case toksPerSec >= 20:
		speedScore = lerp(float64(toksPerSec), 20, 40, 55, 80)
	case toksPerSec >= 10:
		speedScore = lerp(float64(toksPerSec), 10, 20, 35, 55)
	case toksPerSec >= 5:
		speedScore = lerp(float64(toksPerSec), 5, 10, 15, 35)
	case toksPerSec > 0:
		speedScore = lerp(float64(toksPerSec), 0, 5, 0, 15)
	default:
		if fit == FitCanRun {
			speedScore = 45
		} else {
			speedScore = 20
		}
	}

	headroom := 45.0
	switch {
	case memPct <= 20:
		headroom = 100
	case memPct <= 40:
		headroom = lerp(memPct, 20, 40, 100, 75)
	case memPct <= 60:
		headroom = lerp(memPct, 40, 60, 75, 45)
	case memPct <= 80:
		headroom = lerp(memPct, 60, 80, 45, 20)
	default:
		headroom = lerp(math.Min(memPct, 100), 80, 100, 20, 0)
	}

	qualityBonus := math.Min(qualityBonusCap, math.Log2(paramsBillions+1)*2)
	mult := 1.0
	if fit == FitCanRunSlow {
		mult = fitMultiplierSlow
	} else if fit == FitTight {
		mult = fitMultiplierTight
	}
	return int(math.Round((speedScore*scoreWeightSpeed + headroom*scoreWeightHeadroom + qualityBonus) * mult))
}

func lerp(x, x0, x1, y0, y1 float64) float64 {
	if x1 == x0 {
		return y0
	}
	t := (x - x0) / (x1 - x0)
	if t < 0 {
		t = 0
	}
	if t > 1 {
		t = 1
	}
	return y0 + t*(y1-y0)
}

// GradeForScore maps a 0-100 score to a letter grade.
func GradeForScore(s int, fit string) string {
	switch fit {
	case FitCannotRun:
		return "F"
	case FitUnknown:
		return "?"
	case FitCanRunSlow:
		if s >= 40 {
			return "C"
		}
		return "D"
	}
	if fit == FitTight && s < 20 {
		return "D"
	}
	switch {
	case s >= 85:
		return "S"
	case s >= 70:
		return "A"
	case s >= 55:
		return "B"
	case s >= 40:
		return "C"
	case s >= 20:
		return "D"
	default:
		return "F"
	}
}

// AssessItem fills the hardware assessment for one selectable item. Parameters
// come from the model shape when known; otherwise they are estimated from the
// file size and the quant's bits-per-weight.
func AssessItem(item *SelectableItem, hw Hardware, shape ModelShape) {
	if item == nil || hw.VRAMGB <= 0 && hw.SystemRAMGB <= 0 {
		return
	}
	neededGB := float64(item.Size) / (1024 * 1024 * 1024)
	kvPerTok := shape.KVBytesPerToken
	if kvPerTok == 0 {
		kvPerTok = shape.ComputeKVBytesPerToken()
	}
	kvSource := "flat"
	if kvPerTok > 0 && hw.ContextLen > 0 {
		neededGB += float64(kvPerTok) * float64(hw.ContextLen) / (1024 * 1024 * 1024)
		kvSource = "config"
	} else {
		neededGB += FlatOverheadGB
	}
	params := shape.ParamsBillions
	if params <= 0 {
		bpw := bitsPerWeight(item.ID)
		if bpw > 0 {
			params = float64(item.Size) / (bpw / 8) / 1e9
		}
	}

	// MoE: every expert must still be resident, so the fit uses the full size.
	// Decode only touches the active subset, so only the roofline working set is
	// scaled by the active/total ratio.
	speedSet := neededGB
	if params > 0 && shape.ActiveParamsBillions > 0 && shape.ActiveParamsBillions < params {
		speedSet = math.Max(0.5, neededGB*(shape.ActiveParamsBillions/params))
	}

	fit := FitForHardware(neededGB, hw)
	toks := EstimateTokensPerSec(speedSet, hw, neededGB)
	pct := memoryPct(neededGB, hw)
	sc := score(fit, toks, params, pct)

	item.Fit = &HardwareFit{
		Fit:                  fit,
		Grade:                GradeForScore(sc, fit),
		Score:                sc,
		MemoryPct:            round1(pct),
		EstTokensPerSec:      toks,
		NeededGB:             round1(neededGB),
		KVBytesPerToken:      kvPerTok,
		KVSource:             kvSource,
		ActiveParamsBillions: round1(shape.ActiveParamsBillions),
		Reason:               fitReason(fit, sc, toks, pct, hw, kvSource),
	}
}

func fitReason(fit string, sc, toks int, pct float64, hw Hardware, kvSource string) string {
	switch fit {
	case FitCannotRun:
		return "Needs more memory than this machine has"
	case FitCanRunSlow:
		return "Fits only by offloading layers to system RAM"
	case FitTight:
		return "Fits, but with little headroom"
	case FitCanRun:
		if toks >= 40 {
			return "Comfortable fit"
		}
		return "Fits with headroom"
	}
	_ = sc
	_ = kvSource
	return ""
}

func round1(v float64) float64 {
	return math.Round(v*10) / 10
}

// quantBitsPerWeight is the bits-per-weight table for GGUF quantization types.
// Values follow the published GGUF table; unknown names return 0 so callers do
// not invent a number.
var quantBitsPerWeight = map[string]float64{
	"IQ1_S": 1.5625, "IQ1_M": 1.75,
	"IQ2_XXS": 2.0625, "IQ2_XS": 2.3125, "IQ2_S": 2.5, "IQ2_M": 2.25,
	"Q2_K": 2.625, "Q2_K_S": 2.625, "Q2_K_L": 2.625, "Q2_K_XL": 2.625,
	"IQ3_XXS": 3.0625, "IQ3_XS": 3.3125, "IQ3_S": 3.4375, "IQ3_M": 3.5,
	"Q3_K_S": 3.1875, "Q3_K_M": 3.4375, "Q3_K_L": 3.6875, "Q3_K_XL": 3.4375,
	"IQ4_NL": 4.5, "IQ4_XS": 4.25,
	"Q4_0": 4.5, "Q4_1": 4.625, "Q4_K_S": 4.4375, "Q4_K_M": 4.5, "Q4_K_L": 4.5625, "Q4_K_XL": 4.5,
	"Q5_0": 5.5, "Q5_1": 5.5625, "Q5_K_S": 5.3125, "Q5_K_M": 5.5, "Q5_K_L": 5.5625, "Q5_K_XL": 5.5,
	"Q6_K": 6.5625, "Q6_K_L": 6.5625, "Q6_K_XL": 6.5625,
	"Q8_0": 8.5, "Q8_K_XL": 8.5,
	"MXFP4_MOE": 4.0,
	"F16":       16, "BF16": 16, "F32": 32,
}

// bitsPerWeight looks up a quant name, tolerating the "UD_" (unsloth dynamic)
// prefix and the APEX profile names, which have no fixed bpw.
func bitsPerWeight(name string) float64 {
	n := strings.ToUpper(strings.TrimSpace(name))
	if v, ok := quantBitsPerWeight[n]; ok {
		return v
	}
	if v, ok := quantBitsPerWeight[strings.TrimPrefix(n, "UD_")]; ok {
		return v
	}
	return 0
}

// isMTPItem reports whether a selectable item is an MTP (speculative decoding)
// variant. MTP rows are never auto-recommended, matching GGUFToSelectableItems.
func isMTPItem(item *SelectableItem) bool {
	if item == nil {
		return false
	}
	return strings.HasSuffix(strings.ToLower(item.ID), "-mtp") || strings.Contains(item.Label, "(MTP)")
}

// RecommendForHardware assesses every item against hw and moves the
// "recommended" flag to the best quality item that actually fits this machine.
//
// The two tiers are deliberate and can disagree, which is the intended UI:
//   - ★ rec stays quality-first (its existing meaning): among items that fit,
//     the highest quality wins, ties broken by score then by the smaller file.
//   - each item's fit/grade is the hardware view, so a lower-quality quant can
//     grade better than the recommended one.
//
// When nothing fits, the least impossible item is still flagged so the UI points
// at something, and its reason says it does not fit.
//
// Rules that must not change:
//   - MTP variants are never auto-recommended (speculative decoding is a
//     specialist option, see GGUFToSelectableItems).
//   - When hardware is unknown, the existing quality-based rule is left alone.
func RecommendForHardware(items []SelectableItem, hw Hardware, shape ModelShape) []SelectableItem {
	if len(items) == 0 {
		return items
	}
	if hw.VRAMGB <= 0 && hw.SystemRAMGB <= 0 {
		return items
	}

	for i := range items {
		AssessItem(&items[i], hw, shape)
	}

	var best *SelectableItem
	for i := range items {
		it := &items[i]
		if it.Fit == nil {
			continue
		}
		// MTP rows keep the existing rule: never auto-recommended.
		if isMTPItem(it) {
			continue
		}
		if best == nil || betterRecommendation(it, best) {
			best = it
		}
	}
	if best == nil {
		return items
	}
	for i := range items {
		items[i].Recommended = false
	}
	best.Recommended = true
	return items
}

// betterRecommendation orders candidates: fitting beats non-fitting; among
// fitting items quality wins, then score, then the smaller file. When nothing
// fits it points at the smallest file, because that is the least impossible one
// even though its grade is F.
func betterRecommendation(a, b *SelectableItem) bool {
	fits := func(it *SelectableItem) bool {
		return it.Fit != nil && it.Fit.Fit != FitCannotRun && it.Fit.Fit != FitUnknown
	}
	if fits(a) != fits(b) {
		return fits(a)
	}
	if !fits(a) {
		return a.Size < b.Size
	}
	if a.Quality != b.Quality {
		return a.Quality > b.Quality
	}
	if fitScore(a) != fitScore(b) {
		return fitScore(a) > fitScore(b)
	}
	return a.Size < b.Size
}

func fitScore(it *SelectableItem) int {
	if it.Fit == nil {
		return 0
	}
	return it.Fit.Score
}

// ModelShapeFromInfo builds the shape from whatever the repo actually
// declares: config.json for attention geometry, and the GGUF metadata for the
// parameter count when there is no config.json (the usual case for a GGUF-only
// repo). Anything still unknown stays zero, and callers fall back to
// FlatOverheadGB.
func ModelShapeFromInfo(info *RepoInfo) ModelShape {
	if info == nil {
		return ModelShape{}
	}
	var shape ModelShape
	if t := info.Transformers; t != nil {
		shape.Layers = t.NumHiddenLayers
		shape.KVHeads = t.NumKeyValueHeads
		if shape.KVHeads == 0 {
			shape.KVHeads = t.NumAttentionHeads
		}
		shape.HeadDim = t.HeadDim
		if shape.HeadDim == 0 && t.HiddenSize > 0 && t.NumAttentionHeads > 0 {
			shape.HeadDim = t.HiddenSize / t.NumAttentionHeads
		}
	}
	cfg, hasCfg := info.Metadata["config.json"].(map[string]interface{})
	num := func(key string) int {
		if !hasCfg {
			return 0
		}
		if v, ok := cfg[key].(float64); ok {
			return int(v)
		}
		return 0
	}
	// No early return when config.json is missing: a GGUF-only repo usually has
	// none, and the parameter count below comes from the GGUF metadata instead.
	if shape.Layers == 0 {
		shape.Layers = num("num_hidden_layers")
	}
	if shape.KVHeads == 0 {
		shape.KVHeads = num("num_key_value_heads")
		if shape.KVHeads == 0 {
			shape.KVHeads = num("num_attention_heads")
		}
	}
	if shape.HeadDim == 0 {
		shape.HeadDim = num("head_dim")
		if shape.HeadDim == 0 {
			if hs, n := num("hidden_size"), num("num_attention_heads"); hs > 0 && n > 0 {
				shape.HeadDim = hs / n
			}
		}
	}

	shape.ParamsBillions = ParamsBillions(info)
	// Active parameters are only reported when the config declares both counts.
	// This is an approximation: it scales the total by the routing ratio and
	// ignores the attention/shared weights, so it is used for the speed estimate
	// only, never for the fit.
	experts, perTok := num("num_experts"), num("num_experts_per_tok")
	if experts > 0 && perTok > 0 && perTok < experts && shape.ParamsBillions > 0 {
		shape.ActiveParamsBillions = shape.ParamsBillions * float64(perTok) / float64(experts)
	}
	return shape
}

// ParamsBillions returns the parameter count in billions, preferring the parsed
// config value and falling back to the filename-derived count. Returns 0 when
// neither is known.
func ParamsBillions(info *RepoInfo) float64 {
	if info == nil {
		return 0
	}
	if info.Transformers != nil && info.Transformers.EstimatedParametersNum > 0 {
		return float64(info.Transformers.EstimatedParametersNum) / 1e9
	}
	if info.GGUF != nil {
		if n, ok := parseParamCount(info.GGUF.ParameterCount); ok {
			return n
		}
	}
	return 0
}

// parseParamCount reads a count string such as "27B" or "1.5B".
func parseParamCount(s string) (float64, bool) {
	s = strings.ToUpper(strings.TrimSpace(s))
	if s == "" || !strings.HasSuffix(s, "B") {
		return 0, false
	}
	n, err := strconv.ParseFloat(strings.TrimSuffix(s, "B"), 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// AssessEstimatedRepo scores a search result, where the repository name is the
// only fact available: the search API reports no file sizes. The parameter count
// is read from the name and the working set is estimated at Q4_K_M bits per
// weight plus the flat runtime allowance, so this is an estimate, not a measured
// file — callers must label it as such.
//
// The largest size token wins, because MoE names put the active share last
// ("Qwen3-30B-A3B") while fit needs the total. Names with no size token return
// false and are left unranked.
func AssessEstimatedRepo(name string, hw Hardware) (HardwareFit, bool) {
	params, ok := paramCountFromName(name)
	if !ok {
		return HardwareFit{}, false
	}
	needed := params*bitsPerWeight("Q4_K_M")/8.0 + FlatOverheadGB
	fit := FitForHardware(needed, hw)
	toks := EstimateTokensPerSec(needed, hw, needed)
	pct := memoryPct(needed, hw)
	sc := score(fit, toks, params, pct)
	return HardwareFit{
		Fit:             fit,
		Grade:           GradeForScore(sc, fit),
		Score:           sc,
		MemoryPct:       round1(pct),
		EstTokensPerSec: toks,
		NeededGB:        round1(needed),
		KVSource:        "flat",
		ParamsBillions:  params,
		Reason:          fitReason(fit, sc, toks, pct, hw, "flat"),
	}, true
}

// paramCountFromName finds a size token such as "32B" in a repository name and
// returns the largest match.
func paramCountFromName(name string) (float64, bool) {
	best := 0.0
	for i := 0; i < len(name); i++ {
		if !isDigitByte(name[i]) {
			continue
		}
		j := i
		for j < len(name) && isDigitByte(name[j]) {
			j++
		}
		if j < len(name) && name[j] == '.' {
			j++
			for j < len(name) && isDigitByte(name[j]) {
				j++
			}
		}
		if j < len(name) && (name[j] == 'b' || name[j] == 'B') {
			// The size token must end at a word boundary, so "8x7B" reads 7B
			// rather than 8.
			if j+1 >= len(name) || !(isAlphaByte(name[j+1]) || isDigitByte(name[j+1])) {
				if n, ok := parseParamCount(name[i : j+1]); ok && n > best {
					best = n
				}
			}
		}
		i = j
	}
	if best <= 0 {
		return 0, false
	}
	return best, true
}

func isDigitByte(c byte) bool { return c >= '0' && c <= '9' }

func isAlphaByte(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
