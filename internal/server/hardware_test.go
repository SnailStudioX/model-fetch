// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SnailStudioX/model-fetch/pkg/smartdl"
	"github.com/jarvisfriends/devices-common/devices"
)

func TestMergeHardwareFillsFromCatalog(t *testing.T) {
	det := smartdl.Hardware{GPUName: "NVIDIA GeForce RTX 3090", SystemRAMGB: 64, Source: "detected"}
	eff, sources := mergeHardware(det, nil)

	if eff.VRAMGB != 24 || eff.BandwidthGBs != 936 {
		t.Errorf("catalog fill gave vram=%v bw=%v, want 24/936", eff.VRAMGB, eff.BandwidthGBs)
	}
	if sources["vram_gb"] != "catalog" || sources["bandwidth_gbps"] != "catalog" {
		t.Errorf("sources must say catalog, got %v", sources)
	}
	if eff.ContextLen != DefaultContextLen || sources["context_length"] != "default" {
		t.Errorf("context = %d (%s), want %d/default", eff.ContextLen, sources["context_length"], DefaultContextLen)
	}
	if eff.GPUCount != 1 {
		t.Errorf("gpu_count = %d, want 1", eff.GPUCount)
	}
}

func TestMergeHardwareOverrideWins(t *testing.T) {
	det := smartdl.Hardware{GPUName: "NVIDIA GeForce RTX 3090", VRAMGB: 24, BandwidthGBs: 936, SystemRAMGB: 64, GPUCount: 2}
	ov := &HardwareConfig{VRAMGB: 12, ContextLen: 32768, AllowOffload: true}

	eff, sources := mergeHardware(det, ov)
	if eff.VRAMGB != 12 || sources["vram_gb"] != "manual" {
		t.Errorf("override must win: vram=%v source=%v", eff.VRAMGB, sources["vram_gb"])
	}
	if eff.ContextLen != 32768 || sources["context_length"] != "manual" {
		t.Errorf("context override lost: %d/%s", eff.ContextLen, sources["context_length"])
	}
	if !eff.AllowOffload {
		t.Error("allow_offload override lost")
	}
	// Bandwidth is a spec lookup, not a measurement, so it stays labelled
	// catalog even when the value came from the detected adapter name.
	if eff.BandwidthGBs != 936 || sources["bandwidth_gbps"] != "catalog" {
		t.Errorf("bandwidth = %v (%s), want 936/catalog", eff.BandwidthGBs, sources["bandwidth_gbps"])
	}
}

// A device picked in Settings models another machine, so that device's catalog
// specs drive the recommendation even when this machine reports different numbers.
func TestMergeHardwarePickedDeviceDrivesRecommendation(t *testing.T) {
	det := smartdl.Hardware{GPUName: "NVIDIA GeForce RTX 3090", VRAMGB: 24, BandwidthGBs: 936, SystemRAMGB: 64}
	ov := &HardwareConfig{GPUName: "rtx 4060"}

	eff, sources := mergeHardware(det, ov)
	if sources["gpu_name"] != "manual" {
		t.Errorf("gpu override lost: %v", sources["gpu_name"])
	}
	if eff.VRAMGB != 8 || sources["vram_gb"] != "catalog" {
		t.Errorf("picked device specs not applied: vram=%v source=%v", eff.VRAMGB, sources["vram_gb"])
	}
	if eff.BandwidthGBs != 272 || sources["bandwidth_gbps"] != "catalog" {
		t.Errorf("picked device bandwidth not applied: %v/%v", eff.BandwidthGBs, sources["bandwidth_gbps"])
	}
}

// Repeating the detected adapter in another spelling must not replace measured
// numbers with a catalog guess.
func TestMergeHardwareKeepsDetectedValuesWhenOverrideNamesTheSameCard(t *testing.T) {
	det := smartdl.Hardware{GPUName: "NVIDIA GeForce RTX 3090", VRAMGB: 24, BandwidthGBs: 936, SystemRAMGB: 64}
	ov := &HardwareConfig{GPUName: "RTX 3090"}

	eff, sources := mergeHardware(det, ov)
	if eff.VRAMGB != 24 || sources["vram_gb"] != "detected" {
		t.Errorf("same-card override changed values: %v/%v", eff.VRAMGB, sources["vram_gb"])
	}
	if eff.BandwidthGBs != 936 {
		t.Errorf("bandwidth changed: %v", eff.BandwidthGBs)
	}
}

func TestCountRealAdaptersDeduplicatesVendorRows(t *testing.T) {
	snapshot := []devices.Device{
		{Name: "NVIDIA GeForce RTX 3090", ID: "videocontroller3"},
		{Name: "AMD Radeon RX 580 2048SP", ID: "videocontroller4"},
		{Name: "NVIDIA GeForce RTX 3090", ID: "pci:0000:09:00.0"},
		{Name: "OrayIddDriver Device", ID: "videocontroller1"},
		{Name: "MuMu Virtual Display Adapter", ID: "videocontroller2"},
		{Name: "Microsoft Basic Render Driver", ID: "luid:0x14559"},
	}
	if got := countRealAdapters(snapshot); got != 2 {
		t.Errorf("gpu_count = %d, want 2 (one card counted once, virtual adapters skipped)", got)
	}
	if got := countRealAdapters([]devices.Device{{Name: "Microsoft Basic Render Driver"}}); got != 1 {
		t.Errorf("gpu_count with only virtual adapters = %d, want 1", got)
	}
}

// The snapshot can name the same card twice (inventory row plus vendor reading),
// so the list must fold duplicates and skip virtual adapters.
func TestDetectedGPUsListsEveryAdapterBestFirst(t *testing.T) {
	snapshot := []devices.Device{
		{
			Name:   "NVIDIA GeForce RTX 3090",
			ID:     "videocontroller3",
			Fields: []devices.Field{{Key: "vram_total", Value: int64(24 * 1024 * 1024 * 1024)}},
		},
		{
			Name:   "AMD Radeon RX 580 2048SP",
			ID:     "videocontroller4",
			Fields: []devices.Field{{Key: "vram_total", Value: int64(8 * 1024 * 1024 * 1024)}},
		},
		{Name: "NVIDIA GeForce RTX 3090", ID: "pci:0000:09:00.0"},
		{Name: "OrayIddDriver Device", ID: "videocontroller1"},
		{Name: "Microsoft Basic Render Driver", ID: "luid:0x14559"},
	}

	gpus := detectedGPUs(snapshot)
	if len(gpus) != 2 {
		t.Fatalf("detected GPUs = %d, want 2 (vendor duplicate folded, virtual adapters skipped)", len(gpus))
	}
	if gpus[0].Name != "NVIDIA GeForce RTX 3090" || !gpus[0].Selected {
		t.Errorf("first entry must be the selected card, got %q selected=%v", gpus[0].Name, gpus[0].Selected)
	}
	if gpus[0].VRAMGB != 24 || gpus[0].BandwidthGBs != 936 || gpus[0].BandwidthSource != "catalog" {
		t.Errorf("3090 entry = %+v, want 24 GB and catalog bandwidth", gpus[0])
	}
	if gpus[0].Cores != 10496 || gpus[1].Cores != 2304 {
		t.Errorf("core counts = %d/%d, want the catalog specs 10496/2304", gpus[0].Cores, gpus[1].Cores)
	}
	if gpus[1].Name != "AMD Radeon RX 580 2048SP" || gpus[1].VRAMGB != 8 || gpus[1].Selected {
		t.Errorf("second entry = %+v, want the 8 GB card and not selected", gpus[1])
	}
}

// A position beyond the detected adapters is not a measured card, so it stays
// empty; a picked card fills its numbers from the catalog spec.
func TestHardwareSlotsKeepDetectedCardsAndPicks(t *testing.T) {
	det := []DetectedGPU{
		{Name: "NVIDIA GeForce RTX 3090", VRAMGB: 24, BandwidthGBs: 936, Cores: 10496},
		{Name: "AMD Radeon RX 580 2048SP", VRAMGB: 8, BandwidthGBs: 256, Cores: 2304},
	}
	ov := &HardwareConfig{GPUCount: 3, GPUs: []GPUConfig{{}, {Name: "rtx 4060"}}}

	slots := hardwareSlots(det, ov)
	if len(slots) != 3 {
		t.Fatalf("slots = %d, want 3 (two detected plus the count override)", len(slots))
	}
	if slots[0].Name != "NVIDIA GeForce RTX 3090" || slots[0].NameSource != "detected" {
		t.Errorf("slot 1 = %+v, want the detected card", slots[0])
	}
	if slots[1].Name != "rtx 4060" || slots[1].NameSource != "manual" {
		t.Errorf("slot 2 = %+v, want the picked card", slots[1])
	}
	if slots[1].VRAMGB != 8 || slots[1].BandwidthGBs != 272 || slots[1].Cores != 3072 {
		t.Errorf("slot 2 specs = %v/%v/%v, want the rtx 4060 catalog values", slots[1].VRAMGB, slots[1].BandwidthGBs, slots[1].Cores)
	}
	if slots[2].Name != "" || slots[2].NameSource != "unknown" {
		t.Errorf("slot 3 = %+v, want an empty unknown position", slots[2])
	}

	eff, sources := mergeHardware(smartdl.Hardware{GPUName: det[0].Name, VRAMGB: 24, BandwidthGBs: 936, SystemRAMGB: 64}, ov)
	applyPrimarySlot(&eff, sources, slots)
	if eff.GPUName != det[0].Name || eff.GPUCount != 3 {
		t.Errorf("profile must keep slot 1 as the scored card with 3 positions: %+v", eff)
	}
	if sources["cores"] != "catalog" {
		t.Errorf("cores source = %v, want catalog", sources["cores"])
	}
}

// A machine with no adapter at all - a NAS, a VM, a container - still has to
// carry the card the user described. With nothing detected there was no position
// to build, so the slot list narrowed to nothing and the profile reported the
// name and VRAM from the override but no cores.
func TestHardwareSlotsNamedCardWithoutDetectedGPU(t *testing.T) {
	ov := &HardwareConfig{GPUName: "NVIDIA GeForce RTX 3090", VRAMGB: 16}

	slots := hardwareSlots(nil, ov)
	if len(slots) != 1 {
		t.Fatalf("slots = %d, want the one position the override describes", len(slots))
	}
	if slots[0].Name != ov.GPUName || slots[0].NameSource != "manual" {
		t.Errorf("slot 1 = %+v, want the hand-picked card", slots[0])
	}
	if slots[0].Cores != 10496 || slots[0].CoresSource != "catalog" {
		t.Errorf("slot 1 cores = %d (%v), want the 3090 catalog core count from the catalog", slots[0].Cores, slots[0].CoresSource)
	}

	eff, sources := mergeHardware(smartdl.Hardware{SystemRAMGB: 64}, ov)
	applyPrimarySlot(&eff, sources, slots)
	if eff.GPUName != ov.GPUName || eff.BandwidthGBs != 936 {
		t.Errorf("profile = %+v, want the picked card with its catalog bandwidth", eff)
	}
	if sources["cores"] != "catalog" {
		t.Errorf("cores source = %v, want catalog", sources["cores"])
	}
}

// A count lower than the detected adapters is a deliberate choice: the profile
// scores against the strongest card, so the largest-VRAM slot must survive even
// when it is not the first detected row.
func TestHardwareSlotsLowerCountKeepsLargestCard(t *testing.T) {
	det := []DetectedGPU{
		{Name: "AMD Radeon RX 580 2048SP", VRAMGB: 8, BandwidthGBs: 256, Cores: 2304},
		{Name: "NVIDIA GeForce RTX 3090", VRAMGB: 24, BandwidthGBs: 936, Cores: 10496},
	}
	ov := &HardwareConfig{GPUCount: 1}

	slots := hardwareSlots(det, ov)
	if len(slots) != 1 {
		t.Fatalf("slots = %d, want 1", len(slots))
	}
	if slots[0].Name != "NVIDIA GeForce RTX 3090" || slots[0].Index != 1 {
		t.Errorf("slot 1 = %+v, want the largest card renumbered to position 1", slots[0])
	}

	eff, sources := mergeHardware(smartdl.Hardware{GPUName: det[1].Name, VRAMGB: 24, BandwidthGBs: 936, SystemRAMGB: 64}, ov)
	applyPrimarySlot(&eff, sources, slots)
	if eff.GPUName != det[1].Name || eff.GPUCount != 1 {
		t.Errorf("profile must score against the surviving card with one position: %+v", eff)
	}
	if sources["gpu_count"] != "manual" {
		t.Errorf("gpu_count source = %v, want manual", sources["gpu_count"])
	}
}

func TestSanitizeHardwareConfig(t *testing.T) {
	got := sanitizeHardwareConfig(&HardwareConfig{
		GPUName:      "  RTX 3090 ",
		VRAMGB:       -1,
		BandwidthGBs: 99999,
		GPUCount:     20,
		ContextLen:   5_000_000,
	})
	if got == nil {
		t.Fatal("sanitize must not return nil for a non-nil input")
	}
	if got.GPUName != "RTX 3090" {
		t.Errorf("gpu_name = %q, want trimmed", got.GPUName)
	}
	if got.VRAMGB != 0 || got.BandwidthGBs != 0 || got.GPUCount != 0 || got.ContextLen != 0 {
		t.Errorf("out-of-range values must be dropped, got %+v", got)
	}
	if sanitizeHardwareConfig(nil) != nil {
		t.Error("nil input must stay nil")
	}
}

func TestHardwareRouteReportsSources(t *testing.T) {
	srv := New(Config{Hardware: &HardwareConfig{GPUName: "NVIDIA GeForce RTX 3090", VRAMGB: 16}})
	mux := http.NewServeMux()
	srv.registerAPIRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/hardware", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Effective    smartdl.Hardware  `json:"effective"`
		Sources      map[string]string `json:"sources"`
		Gaps         []string          `json:"gaps"`
		DetectedGPUs []DetectedGPU     `json:"detected_gpus"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if resp.Effective.VRAMGB != 16 || resp.Sources["vram_gb"] != "manual" {
		t.Errorf("override must survive the route: vram=%v source=%v", resp.Effective.VRAMGB, resp.Sources["vram_gb"])
	}
	if resp.Effective.BandwidthGBs != 936 {
		t.Errorf("bandwidth catalog fill lost: %v", resp.Effective.BandwidthGBs)
	}
	if resp.DetectedGPUs == nil {
		t.Error("detected_gpus must always be an array, even when nothing is detected")
	}

	// gpu_cores is reported even when the catalog has no core figure, so the UI can
	// distinguish "no cores known" from "the field is missing".
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if raw["gpu_cores"] != float64(10496) {
		t.Errorf("gpu_cores = %v, want the 3090 catalog core count", raw["gpu_cores"])
	}
	if _, ok := raw["gpus"]; !ok {
		t.Error("gpus must always be present, even with a single card")
	}
	if raw["sources"] != nil && raw["sources"].(map[string]any)["cores"] != "catalog" {
		t.Errorf("cores source must be catalog, got %v", raw["sources"])
	}
}

func TestRankSearchResultsByHardware(t *testing.T) {
	hw := smartdl.Hardware{VRAMGB: 24, SystemRAMGB: 64, BandwidthGBs: 936}
	results := []SearchResult{
		{ID: "bigmodel/GLM-4.5"},         // no size token: unranked, lands after the scored rows
		{ID: "Qwen/Qwen3.8-27B"},         // can-run
		{ID: "meta-llama/Llama-3.3-70B"}, // cannot-run on 24 GB
	}
	rankSearchResultsByHardware(results, hw)
	if results[0].ID != "Qwen/Qwen3.8-27B" {
		t.Errorf("first = %q, want the fitting 27B", results[0].ID)
	}
	if results[1].ID != "meta-llama/Llama-3.3-70B" {
		t.Errorf("second = %q, want the scored-but-cannot-run 70B", results[1].ID)
	}
	if results[2].Fit != nil {
		t.Errorf("unscored row must keep a nil fit, got %+v", results[2].Fit)
	}

	// Unknown hardware must not invent a ranking.
	untouched := []SearchResult{{ID: "Qwen/Qwen3.8-27B"}, {ID: "bigmodel/GLM-4.5"}}
	rankSearchResultsByHardware(untouched, smartdl.Hardware{})
	if untouched[0].ID != "Qwen/Qwen3.8-27B" || untouched[0].Fit != nil {
		t.Error("unknown hardware must leave the upstream order and no fit")
	}
}
