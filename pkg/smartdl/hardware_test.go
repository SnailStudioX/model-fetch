package smartdl

import (
	"math"
	"strings"
	"testing"
)

func TestLookupBandwidthAndVRAM(t *testing.T) {
	cases := []struct {
		name string
		bw   float64
		vram float64
	}{
		{"NVIDIA GeForce RTX 3090", 936, 24},
		{"AMD Radeon RX 580 2048SP", 256, 8},
		{"NVIDIA GeForce RTX 3080 12GB", 912, 12},
		{"Some Unknown Card", 0, 0},
	}
	for _, c := range cases {
		if got := LookupBandwidth(c.name); got != c.bw {
			t.Errorf("LookupBandwidth(%q) = %v, want %v", c.name, got, c.bw)
		}
		if got := LookupVRAM(c.name); got != c.vram {
			t.Errorf("LookupVRAM(%q) = %v, want %v", c.name, got, c.vram)
		}
	}
}

func TestFitThresholds(t *testing.T) {
	hw := Hardware{VRAMGB: 24, SystemRAMGB: 64, BandwidthGBs: 936}

	if got := FitForHardware(17, hw); got != FitCanRun {
		t.Errorf("17 GB on 24 GB: got %q, want can-run (threshold 20.4)", got)
	}
	if got := FitForHardware(26, hw); got != FitTight {
		t.Errorf("26 GB on 24 GB: got %q, want tight (threshold 26.4)", got)
	}
	if got := FitForHardware(40, hw); got != FitCannotRun {
		t.Errorf("40 GB on 24 GB without offload: got %q, want cannot-run", got)
	}
	off := hw
	off.AllowOffload = true
	if got := FitForHardware(40, off); got != FitCanRunSlow {
		t.Errorf("40 GB on 24 GB with offload: got %q, want can-run-slow (budget 68.8)", got)
	}
	unknown := Hardware{}
	if got := FitForHardware(10, unknown); got != FitUnknown {
		t.Errorf("unknown hardware: got %q, want unknown", got)
	}

	unified := Hardware{SystemRAMGB: 64, Unified: true, BandwidthGBs: 400}
	// usable = 48, can-run <= 33.6, tight <= 48
	if got := FitForHardware(30, unified); got != FitCanRun {
		t.Errorf("30 GB on unified 64: got %q, want can-run", got)
	}
	if got := FitForHardware(40, unified); got != FitTight {
		t.Errorf("40 GB on unified 64: got %q, want tight", got)
	}
}

func TestModelShapeComputesAndFallsBackToFlat(t *testing.T) {
	// 2 * 48 layers * 8 kv heads * 128 head dim * 2 bytes = 196608 bytes/token
	shape := ModelShape{Layers: 48, KVHeads: 8, HeadDim: 128}
	if got := shape.ComputeKVBytesPerToken(); got != 196608 {
		t.Errorf("KV bytes/token = %d, want 196608", got)
	}
	if got := (ModelShape{Layers: 48}).ComputeKVBytesPerToken(); got != 0 {
		t.Errorf("incomplete shape should return 0, got %d", got)
	}

	hw := Hardware{VRAMGB: 24, SystemRAMGB: 64, BandwidthGBs: 936, ContextLen: 32768}
	item := SelectableItem{ID: "q4_k_m", Size: 17 * 1024 * 1024 * 1024}
	AssessItem(&item, hw, shape)
	if item.Fit == nil {
		t.Fatal("AssessItem produced no fit")
	}
	if item.Fit.KVSource != "config" {
		t.Errorf("kv_source = %q, want config", item.Fit.KVSource)
	}
	// 17 + 196608*32768/1GiB = 17 + 6.0 = 23.0 GB
	if math.Abs(item.Fit.NeededGB-23.0) > 0.1 {
		t.Errorf("needed_gb = %v, want ~23.0 (17 weights + 6.0 KV)", item.Fit.NeededGB)
	}

	// Without a shape the flat overhead is used and the source says so.
	item2 := SelectableItem{ID: "q4_k_m", Size: 17 * 1024 * 1024 * 1024}
	AssessItem(&item2, hw, ModelShape{})
	if item2.Fit == nil || item2.Fit.KVSource != "flat" {
		t.Fatalf("expected flat kv_source, got %+v", item2.Fit)
	}
	if math.Abs(item2.Fit.NeededGB-17.5) > 0.05 {
		t.Errorf("needed_gb = %v, want 17.5 with flat overhead", item2.Fit.NeededGB)
	}
}

func TestRooflineSpeedAndOffload(t *testing.T) {
	hw := Hardware{VRAMGB: 24, SystemRAMGB: 64, BandwidthGBs: 936}
	if got := EstimateTokensPerSec(17, hw, 17); got != 39 {
		t.Errorf("resident 17 GB: got %d tok/s, want 39 (936/17*0.70 = 38.54)", got)
	}
	// Offload: 30 GB working set, 24 resident in VRAM, rest in system RAM.
	// effective BW = 1 / (24/30/936 + 6/30/50) = 1 / (0.000854 + 0.004) = 198 GB/s
	// 198/30 * 0.70 * 0.85 = 3.9 -> 4
	if got := EstimateTokensPerSec(30, hw, 30); got != 4 {
		t.Errorf("offload 30 GB: got %d tok/s, want 4", got)
	}
	if got := EstimateTokensPerSec(17, Hardware{VRAMGB: 24}, 17); got != 0 {
		t.Errorf("no bandwidth must return 0, got %d", got)
	}
}

func TestScoreAndGradeWorkedExample(t *testing.T) {
	hw := Hardware{VRAMGB: 24, SystemRAMGB: 64, BandwidthGBs: 936, ContextLen: 4096}
	item := SelectableItem{ID: "q4_k_m", Size: 17 * 1024 * 1024 * 1024}
	AssessItem(&item, hw, ModelShape{ParamsBillions: 27})
	if item.Fit == nil {
		t.Fatal("no fit")
	}
	if item.Fit.Fit != FitCanRun {
		t.Errorf("fit = %q, want can-run", item.Fit.Fit)
	}
	// Flat overhead applies (no KV shape): needed = 17.5 GB.
	// speed = 936/17.5*0.70 = 37.4 -> 37; headroom 17.5/24 = 72.9% -> 28.85;
	// speedScore 76.25; qualityBonus min(12, log2(28)*2) = 9.61.
	// (76.25*0.55 + 28.85*0.35 + 9.61) = 61.65 -> 62
	if item.Fit.EstTokensPerSec != 37 {
		t.Errorf("est_tokens_per_sec = %d, want 37", item.Fit.EstTokensPerSec)
	}
	if item.Fit.Score != 62 {
		t.Errorf("score = %d, want 62", item.Fit.Score)
	}
	if item.Fit.Grade != "B" {
		t.Errorf("grade = %q, want B", item.Fit.Grade)
	}
	if item.Fit.Reason == "" {
		t.Error("reason must be non-empty for an assessed item")
	}
}

func TestRecommendForHardwareMovesFlagAndKeepsMTPRule(t *testing.T) {
	hw := Hardware{VRAMGB: 8, SystemRAMGB: 64, BandwidthGBs: 256, ContextLen: 4096}
	items := []SelectableItem{
		{ID: "q4_k_m", Label: "Q4_K_M", Size: 17 * 1024 * 1024 * 1024, Quality: 4, Recommended: true},
		{ID: "q2_k", Label: "Q2_K", Size: 5 * 1024 * 1024 * 1024, Quality: 1},
		{ID: "q4_k_m-mtp", Label: "Q4_K_M (MTP)", Size: 17 * 1024 * 1024 * 1024, Quality: 4},
	}
	out := RecommendForHardware(items, hw, ModelShape{ParamsBillions: 27})

	var q2 *SelectableItem
	for i := range out {
		if out[i].ID == "q2_k" {
			q2 = &out[i]
		}
		if strings.HasSuffix(out[i].ID, "-mtp") && out[i].Recommended {
			t.Errorf("MTP item %q must never be auto-recommended", out[i].ID)
		}
	}
	if q2 == nil || !q2.Recommended {
		t.Errorf("the only fitting item should be recommended; got %+v", q2)
	}

	// Unknown hardware leaves the original flag untouched.
	untouched := []SelectableItem{{ID: "q4_k_m", Recommended: true}}
	out2 := RecommendForHardware(untouched, Hardware{}, ModelShape{})
	if !out2[0].Recommended || out2[0].Fit != nil {
		t.Errorf("unknown hardware must not change recommendation or add fit: %+v", out2[0])
	}
}

func TestRecommendForHardwareKeepsQualityFirstAmongFittingItems(t *testing.T) {
	hw := Hardware{VRAMGB: 24, SystemRAMGB: 64, BandwidthGBs: 936, ContextLen: 8192}
	items := []SelectableItem{
		{ID: "q4_k_m", Label: "Q4_K_M", Size: 5800 * 1024 * 1024, Quality: 4},
		{ID: "iq3_xxs", Label: "IQ3_XXS", Size: 4300 * 1024 * 1024, Quality: 3},
	}
	out := RecommendForHardware(items, hw, ModelShape{ParamsBillions: 8})

	// Both fit comfortably, so ★ rec stays the better-quality file even though
	// the smaller one scores higher on speed and headroom.
	rec := ""
	for _, it := range out {
		if it.Recommended {
			rec = it.ID
		}
	}
	if rec != "q4_k_m" {
		t.Errorf("recommended = %q, want q4_k_m (quality-first among fitting items)", rec)
	}
	for _, it := range out {
		if it.Fit == nil || it.Fit.Fit != FitCanRun {
			t.Errorf("fit missing or wrong for %q: %+v", it.ID, it.Fit)
		}
	}

	// When nothing fits, the least impossible item is still flagged.
	small := []SelectableItem{
		{ID: "q4_k_m", Label: "Q4_K_M", Size: 17 * 1024 * 1024 * 1024, Quality: 4},
		{ID: "q2_k", Label: "Q2_K", Size: 5 * 1024 * 1024 * 1024, Quality: 1},
	}
	tiny := Hardware{VRAMGB: 4, SystemRAMGB: 8, BandwidthGBs: 256, ContextLen: 4096}
	out2 := RecommendForHardware(small, tiny, ModelShape{ParamsBillions: 27})
	if !out2[1].Recommended {
		t.Errorf("nothing fits, so the smaller item should be flagged: %+v", out2[1])
	}
	if out2[1].Fit == nil || out2[1].Fit.Fit == FitUnknown {
		t.Errorf("fit must still be reported when nothing fits: %+v", out2[1].Fit)
	}
}

func TestMoESpeedUsesActiveParamsButFitUsesFullWeights(t *testing.T) {
	hw := Hardware{VRAMGB: 24, SystemRAMGB: 64, BandwidthGBs: 936, ContextLen: 4096}
	// A 30B-total / 3.3B-active MoE: every expert must still be resident, so the
	// fit sees the full 18 GB, while decode only touches the active share.
	shape := ModelShape{ParamsBillions: 30.5, ActiveParamsBillions: 3.3}
	item := SelectableItem{ID: "ud_q4_k_xl", Size: 18 * 1024 * 1024 * 1024}
	AssessItem(&item, hw, shape)
	if item.Fit == nil {
		t.Fatal("no fit")
	}
	if math.Abs(item.Fit.NeededGB-18.5) > 0.05 {
		t.Errorf("needed_gb = %v, want 18.5 (all experts resident + flat overhead)", item.Fit.NeededGB)
	}
	// 18.5 * 3.3/30.5 = 2.0 GB working set -> 936/2.0*0.70 = 327, capped at 100.
	if item.Fit.EstTokensPerSec != 100 {
		t.Errorf("est_tokens_per_sec = %d, want 100 (roofline capped)", item.Fit.EstTokensPerSec)
	}
	if item.Fit.Score != 73 {
		t.Errorf("score = %d, want 73 (59 without the active-param split)", item.Fit.Score)
	}
	if item.Fit.Grade != "A" {
		t.Errorf("grade = %q, want A", item.Fit.Grade)
	}
}

func TestBitsPerWeight(t *testing.T) {
	if got := bitsPerWeight("Q4_K_M"); got != 4.5 {
		t.Errorf("Q4_K_M = %v, want 4.5", got)
	}
	if got := bitsPerWeight("UD_Q4_K_XL"); got != 4.5 {
		t.Errorf("UD_Q4_K_XL = %v, want 4.5", got)
	}
	if got := bitsPerWeight("APEX_BALANCED"); got != 0 {
		t.Errorf("APEX profile has no fixed bpw, got %v", got)
	}
}

func TestParamCountFromNameUsesLargestToken(t *testing.T) {
	cases := []struct {
		name string
		want float64
		ok   bool
	}{
		{"Qwen/Qwen3.8-27B", 27, true},
		// Largest, not last: fit needs the total, not the per-token share.
		{"Qwen/Qwen3-30B-A3B", 30, true},
		{"deepseek-ai/DeepSeek-R1-Distill-Qwen-32B", 32, true},
		// "8x7B" has no size token on the 8, so the 7B is the match.
		{"mistralai/Mixtral-8x7B-v0.1", 7, true},
		{"google/gemma-3-27b-it", 27, true},
		{"TinyLlama/TinyLlama-1.1B-Chat-v1.0", 1.1, true},
		{"microsoft/phi-4", 0, false},
		{"unsloth/Qwen3-8B-GGUF", 8, true},
	}
	for _, c := range cases {
		got, ok := paramCountFromName(c.name)
		if ok != c.ok || got != c.want {
			t.Errorf("paramCountFromName(%q) = %v, %v; want %v, %v", c.name, got, ok, c.want, c.ok)
		}
	}
}

func TestAssessEstimatedRepo(t *testing.T) {
	hw := Hardware{VRAMGB: 24, SystemRAMGB: 64, BandwidthGBs: 936}
	fit, ok := AssessEstimatedRepo("Qwen/Qwen3.8-27B", hw)
	if !ok {
		t.Fatal("27B name must be assessable")
	}
	// needed = 27*4.5/8 + 0.5 = 15.6875 GB; speed = 936/15.6875*0.70 = 41.8 -> 42.
	if fit.ParamsBillions != 27 || fit.NeededGB != 15.7 || fit.EstTokensPerSec != 42 {
		t.Errorf("got params=%v needed=%v tok/s=%d", fit.ParamsBillions, fit.NeededGB, fit.EstTokensPerSec)
	}
	if fit.Fit != FitCanRun || fit.Grade != "B" || fit.Score != 68 {
		t.Errorf("got fit=%q grade=%q score=%d", fit.Fit, fit.Grade, fit.Score)
	}
	if fit.KVSource != "flat" {
		t.Errorf("kv_source = %q, want flat", fit.KVSource)
	}
	if _, ok := AssessEstimatedRepo("bigmodel/GLM-4.5", hw); ok {
		t.Error("a name with no size token must not be scored")
	}
}
