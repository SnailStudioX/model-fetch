// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/jarvisfriends/devices-common/devices"
	"github.com/jarvisfriends/devices-gpu/gpu"
	"github.com/jaypipes/ghw"

	"github.com/SnailStudioX/model-fetch/pkg/smartdl"
)

// HardwareConfig is the user's override block. Every field is optional; a value
// left empty means "use what was detected". Overrides always win, because the
// detector cannot see every card (and cannot see bandwidth at all).
// GPUConfig is one GPU position in the override block. A blank name means "use
// the card detected in that position"; the numbers default to the catalog spec
// for the picked card, so picking a device fills VRAM, bandwidth and cores.
type GPUConfig struct {
	Name         string  `json:"name,omitempty" yaml:"name,omitempty"`
	VRAMGB       float64 `json:"vram_gb,omitempty" yaml:"vram_gb,omitempty"`
	BandwidthGBs float64 `json:"bandwidth_gbps,omitempty" yaml:"bandwidth_gbps,omitempty"`
	Cores        int     `json:"cores,omitempty" yaml:"cores,omitempty"`
}

type HardwareConfig struct {
	GPUName      string  `json:"gpu_name,omitempty" yaml:"gpu_name,omitempty"`
	VRAMGB       float64 `json:"vram_gb,omitempty" yaml:"vram_gb,omitempty"`
	SystemRAMGB  float64 `json:"system_ram_gb,omitempty" yaml:"system_ram_gb,omitempty"`
	BandwidthGBs float64 `json:"bandwidth_gbps,omitempty" yaml:"bandwidth_gbps,omitempty"`
	GPUCount     int     `json:"gpu_count,omitempty" yaml:"gpu_count,omitempty"`
	ContextLen   int     `json:"context_length,omitempty" yaml:"context_length,omitempty"`
	AllowOffload bool    `json:"allow_offload,omitempty" yaml:"allow_offload,omitempty"`
	// GPUs is the per-position override, in slot order. It wins over the
	// single-field override when both are present.
	GPUs []GPUConfig `json:"gpus,omitempty" yaml:"gpus,omitempty"`
}

// DefaultContextLen is the context assumed for the KV-cache budget when the
// user has not set one. It is a budget, not a model limit.
const DefaultContextLen = 8192

// maxHardwareOverride caps hand-entered values so a typo cannot make every
// model look runnable.
const maxHardwareOverride = 1024.0

// maxGPUCount caps the number of GPU positions a profile can describe.
const maxGPUCount = 8

// sanitizeHardwareConfig drops non-positive and absurd values rather than
// trusting whatever the caller sent.
func sanitizeHardwareConfig(h *HardwareConfig) *HardwareConfig {
	if h == nil {
		return nil
	}
	out := *h
	out.GPUName = strings.TrimSpace(out.GPUName)
	if out.VRAMGB < 0 || out.VRAMGB > maxHardwareOverride {
		out.VRAMGB = 0
	}
	if out.SystemRAMGB < 0 || out.SystemRAMGB > maxHardwareOverride {
		out.SystemRAMGB = 0
	}
	if out.BandwidthGBs < 0 || out.BandwidthGBs > 20000 {
		out.BandwidthGBs = 0
	}
	if out.GPUCount < 0 || out.GPUCount > 8 {
		out.GPUCount = 0
	}
	if out.ContextLen < 0 || out.ContextLen > 1_000_000 {
		out.ContextLen = 0
	}
	if len(out.GPUs) > maxGPUCount {
		out.GPUs = out.GPUs[:maxGPUCount]
	}
	for i := range out.GPUs {
		out.GPUs[i].Name = strings.TrimSpace(out.GPUs[i].Name)
		if out.GPUs[i].VRAMGB < 0 || out.GPUs[i].VRAMGB > maxHardwareOverride {
			out.GPUs[i].VRAMGB = 0
		}
		if out.GPUs[i].BandwidthGBs < 0 || out.GPUs[i].BandwidthGBs > 20000 {
			out.GPUs[i].BandwidthGBs = 0
		}
		if out.GPUs[i].Cores < 0 || out.GPUs[i].Cores > 200000 {
			out.GPUs[i].Cores = 0
		}
	}
	return &out
}

// hardwareReading is one detection pass. Detected is what the machine reports;
// VRAMUsedGB is only present at Detailed level.
type hardwareReading struct {
	Detected     smartdl.Hardware
	DetectedGPUs []DetectedGPU
	VRAMUsedGB   float64
	Gaps         []string
	Taken        time.Time
}

// virtualAdapterNames are adapters that are not real compute devices and must
// not be treated as a GPU for fit purposes.
var virtualAdapterNames = []string{
	"basic render driver",
	"virtual display",
	"orayidd",
	"microsoft",
}

func isVirtualAdapter(name string) bool {
	n := strings.ToLower(name)
	for _, v := range virtualAdapterNames {
		if strings.Contains(n, v) {
			return true
		}
	}
	return false
}

// hardwareEngine is created lazily so a Server built without New (tests) still
// works. The Engine caches static values, which is what makes repeated strip
// refreshes cheap.
func (s *Server) hardwareEngine() *devices.Engine {
	s.hwMu.Lock()
	defer s.hwMu.Unlock()
	if s.hwEngine == nil {
		s.hwEngine = devices.New(gpu.New())
	}
	return s.hwEngine
}

// detectHardware runs one pass. Basic is the path used by the strip: the static
// inventory is cached, but the vendor reading still runs nvidia-smi, which costs
// about half a second per uncached call. Detailed adds the WDDM performance
// counter sample, measured at about 3.5 s, so it must not run on every analyze.
func (s *Server) detectHardware(ctx context.Context, level devices.Level) hardwareReading {
	reading := hardwareReading{Taken: time.Now()}

	eng := s.hardwareEngine()
	snap, err := eng.Snapshot(ctx, level)
	if err != nil {
		reading.Gaps = append(reading.Gaps, "gpu: "+err.Error())
	}

	reading.DetectedGPUs = detectedGPUs(snap.Devices)
	for _, d := range snap.Devices {
		if isVirtualAdapter(d.Name) {
			continue
		}
		used, _ := fieldFloat(d, "vram_used")
		if used > reading.VRAMUsedGB {
			reading.VRAMUsedGB = used / 1024 / 1024 / 1024
		}
	}

	if len(reading.DetectedGPUs) > 0 {
		best := reading.DetectedGPUs[0]
		reading.Detected.GPUName = best.Name
		reading.Detected.GPUCount = countRealAdapters(snap.Devices)
		reading.Detected.VRAMGB = best.VRAMGB
		// An adapter with no dedicated memory is treated as unified memory.
		if reading.Detected.VRAMGB == 0 {
			reading.Detected.Unified = true
		}
	} else {
		reading.Gaps = append(reading.Gaps, "no physical GPU adapter found")
	}

	// Bandwidth is never measured; it is a spec lookup on the detected name.
	if bw := smartdl.LookupBandwidth(reading.Detected.GPUName); bw > 0 {
		reading.Detected.BandwidthGBs = bw
	} else {
		reading.Gaps = append(reading.Gaps, "bandwidth unknown for "+reading.Detected.GPUName)
	}

	if mem, err := ghw.Memory(ghw.WithDisableWarnings()); err == nil && mem.TotalPhysicalBytes > 0 {
		reading.Detected.SystemRAMGB = float64(mem.TotalPhysicalBytes) / 1024 / 1024 / 1024
	} else {
		reading.Gaps = append(reading.Gaps, "system RAM unknown")
	}

	reading.Detected.Source = "detected"
	return reading
}

// DetectedGPU is one physical adapter from the snapshot. The same card can show
// up twice — once from the inventory row and once from the vendor reading — so
// entries are deduplicated by name. Bandwidth and core count are always the
// catalog specs, because neither is measurable from the OS.
type DetectedGPU struct {
	Name            string  `json:"name"`
	VRAMGB          float64 `json:"vram_gb,omitempty"`
	BandwidthGBs    float64 `json:"bandwidth_gbps,omitempty"`
	BandwidthSource string  `json:"bandwidth_source,omitempty"`
	Cores           int     `json:"cores,omitempty"`
	Selected        bool    `json:"selected,omitempty"`
}

// detectedGPUs lists every physical adapter, best first. "Best" uses the same
// rule as the machine profile pick: the larger of the catalog spec and the VRAM
// the device itself reports.
func detectedGPUs(devicesIn []devices.Device) []DetectedGPU {
	byName := map[string]*DetectedGPU{}
	order := []string{}
	for _, d := range devicesIn {
		if isVirtualAdapter(d.Name) {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(d.Name))
		entry, ok := byName[key]
		if !ok {
			entry = &DetectedGPU{Name: strings.TrimSpace(d.Name)}
			byName[key] = entry
			order = append(order, key)
		}
		if vram, ok := fieldFloat(d, "vram_total"); ok && vram > 0 {
			entry.VRAMGB = max(entry.VRAMGB, vram/1024/1024/1024)
		}
	}

	sort.SliceStable(order, func(i, j int) bool {
		return gpuScore(byName[order[i]]) > gpuScore(byName[order[j]])
	})

	out := make([]DetectedGPU, 0, len(order))
	for i, key := range order {
		entry := byName[key]
		if entry.VRAMGB == 0 {
			entry.VRAMGB = smartdl.LookupVRAM(entry.Name)
		}
		if bw := smartdl.LookupBandwidth(entry.Name); bw > 0 {
			entry.BandwidthGBs = bw
			entry.BandwidthSource = "catalog"
		}
		entry.Cores = smartdl.LookupCores(entry.Name)
		entry.Selected = i == 0
		out = append(out, *entry)
	}
	return out
}

// gpuScore ranks an adapter by the larger of its measured VRAM and its catalog
// spec, so a card the catalog does not know is still ranked by what it reports.
func gpuScore(g *DetectedGPU) float64 {
	spec := smartdl.LookupVRAM(g.Name)
	if g.VRAMGB > spec {
		return g.VRAMGB
	}
	return spec
}

// countRealAdapters counts distinct physical GPUs. One adapter can appear twice
// in a snapshot — once from the inventory row and once from the vendor reading,
// which is keyed by PCI address — so counting rows would report the same card
// twice. Names are compared case-insensitively.
func countRealAdapters(devicesIn []devices.Device) int {
	seen := map[string]bool{}
	for _, d := range devicesIn {
		if isVirtualAdapter(d.Name) {
			continue
		}
		seen[strings.ToLower(strings.TrimSpace(d.Name))] = true
	}
	if len(seen) == 0 {
		return 1
	}
	return len(seen)
}

func fieldFloat(d devices.Device, key string) (float64, bool) {
	for _, f := range d.Fields {
		if f.Key == key {
			return f.Float()
		}
	}
	return 0, false
}

// effectiveHardware merges the detected machine with the user's overrides and
// records which source each number came from, so the UI can say it.
// GPUSlot is one GPU position in the machine profile: the detected card, or the
// card picked for that position. Each number records its own source so the UI can
// say whether a value is measured, hand-entered, or a catalog spec.
type GPUSlot struct {
	Index           int     `json:"index"`
	Name            string  `json:"name"`
	VRAMGB          float64 `json:"vram_gb,omitempty"`
	BandwidthGBs    float64 `json:"bandwidth_gbps,omitempty"`
	Cores           int     `json:"cores,omitempty"`
	NameSource      string  `json:"name_source"`
	VRAMSource      string  `json:"vram_source"`
	BandwidthSource string  `json:"bandwidth_source"`
	CoresSource     string  `json:"cores_source"`
}

// hardwareSlots builds one slot per GPU position. Positions come from the
// detected adapters, widened by the override when it describes more cards. A
// picked card models a different machine, so its catalog specs fill the numbers
// unless the override carries its own.
//
// An explicit count can also be lower than the detected adapters: the profile
// then scores against the strongest cards only, so the largest-VRAM slots survive.
func hardwareSlots(det []DetectedGPU, ov *HardwareConfig) []GPUSlot {
	described := len(det)
	if ov != nil && len(ov.GPUs) > described {
		described = len(ov.GPUs)
	}
	target := described
	if ov != nil && ov.GPUCount > 0 {
		// An explicit count wins in both directions: it widens the profile with
		// empty positions, or narrows it to the strongest cards.
		target = ov.GPUCount
	}
	count := max(described, target)
	if count == 0 {
		count = 1
	}

	slots := make([]GPUSlot, 0, count)
	for i := 0; i < count; i++ {
		slot := GPUSlot{Index: i + 1, NameSource: "detected", VRAMSource: "detected", BandwidthSource: "catalog", CoresSource: "catalog"}
		if i < len(det) {
			base := det[i]
			slot.Name = base.Name
			slot.VRAMGB = base.VRAMGB
			slot.BandwidthGBs = base.BandwidthGBs
			slot.Cores = base.Cores
			if slot.VRAMGB == 0 {
				slot.VRAMSource = "catalog"
			}
		} else {
			// A position beyond the detected adapters is not a measured card, so it
			// stays empty until the user picks one.
			slot.NameSource = "unknown"
			slot.VRAMSource = "unknown"
			slot.BandwidthSource = "unknown"
			slot.CoresSource = "unknown"
		}
		if ov == nil {
			slots = append(slots, slot)
			continue
		}
		var pick GPUConfig
		if i < len(ov.GPUs) {
			pick = ov.GPUs[i]
		} else if i == 0 {
			// The single-field override still describes the first position, so an
			// older profile keeps working without a per-slot entry.
			pick = GPUConfig{Name: ov.GPUName, VRAMGB: ov.VRAMGB, BandwidthGBs: ov.BandwidthGBs}
		}
		if pick.Name != "" {
			slot.Name = pick.Name
			slot.NameSource = "manual"
			if e, ok := smartdl.LookupCatalogEntry(pick.Name); ok {
				slot.VRAMGB = e.VRAMGB
				slot.BandwidthGBs = e.BandwidthGBs
				slot.Cores = e.Cores
				slot.VRAMSource = "catalog"
				slot.CoresSource = "catalog"
			} else {
				slot.VRAMGB = 0
				slot.BandwidthGBs = 0
				slot.Cores = 0
				slot.VRAMSource = "unknown"
				slot.BandwidthSource = "unknown"
				slot.CoresSource = "unknown"
			}
		}
		if pick.VRAMGB > 0 {
			slot.VRAMGB = pick.VRAMGB
			slot.VRAMSource = "manual"
		}
		if pick.BandwidthGBs > 0 {
			slot.BandwidthGBs = pick.BandwidthGBs
			slot.BandwidthSource = "manual"
		}
		if pick.Cores > 0 {
			slot.Cores = pick.Cores
			slot.CoresSource = "manual"
		}
		slots = append(slots, slot)
	}

	if target < len(slots) {
		// Narrowing keeps the biggest cards, renumbered so position 1 is the card the
		// fit model scores against.
		sort.SliceStable(slots, func(i, j int) bool { return slots[i].VRAMGB > slots[j].VRAMGB })
		slots = slots[:target]
		for i := range slots {
			slots[i].Index = i + 1
		}
	}
	return slots
}

// mergeHardware folds the user's overrides onto the detected machine and records
// which source each number came from, so the UI can say it. Pure: no device
// access, which keeps it testable.
func mergeHardware(det smartdl.Hardware, ov *HardwareConfig) (smartdl.Hardware, map[string]string) {
	eff := det
	sources := map[string]string{}
	for _, k := range []string{"gpu_name", "vram_gb", "system_ram_gb", "gpu_count"} {
		sources[k] = "detected"
	}
	// Bandwidth is never measured by the device layer; it is a spec lookup on
	// the adapter name, so it is labelled catalog unless the user typed it.
	sources["bandwidth_gbps"] = "catalog"

	if ov != nil {
		if ov.GPUName != "" {
			eff.GPUName = ov.GPUName
			sources["gpu_name"] = "manual"
		}
		if ov.VRAMGB > 0 {
			eff.VRAMGB = ov.VRAMGB
			sources["vram_gb"] = "manual"
		}
		if ov.SystemRAMGB > 0 {
			eff.SystemRAMGB = ov.SystemRAMGB
			sources["system_ram_gb"] = "manual"
		}
		if ov.BandwidthGBs > 0 {
			eff.BandwidthGBs = ov.BandwidthGBs
			sources["bandwidth_gbps"] = "manual"
		}
		if ov.GPUCount > 0 {
			eff.GPUCount = ov.GPUCount
			sources["gpu_count"] = "manual"
		}
		eff.AllowOffload = ov.AllowOffload
		if ov.ContextLen > 0 {
			eff.ContextLen = ov.ContextLen
			sources["context_length"] = "manual"
		}
	}

	// Fill what detection could not provide from the spec catalog, and say so.
	// A device picked in Settings models a different machine, so its catalog
	// specs drive the recommendation; when the override just repeats this
	// machine's own adapter, the detected numbers stay authoritative.
	if eff.GPUName != "" {
		if e, ok := smartdl.LookupCatalogEntry(eff.GPUName); ok {
			pickedOther := ov != nil && ov.GPUName != "" && !smartdl.SameDeviceName(ov.GPUName, det.GPUName)
			if e.VRAMGB > 0 && (pickedOther && ov.VRAMGB == 0 || eff.VRAMGB == 0) {
				eff.VRAMGB = e.VRAMGB
				sources["vram_gb"] = "catalog"
			}
			if e.BandwidthGBs > 0 && (pickedOther && ov.BandwidthGBs == 0 || eff.BandwidthGBs == 0) {
				eff.BandwidthGBs = e.BandwidthGBs
				sources["bandwidth_gbps"] = "catalog"
			}
		}
	}
	if eff.SystemRAMGB == 0 {
		sources["system_ram_gb"] = "unknown"
	}
	if eff.ContextLen == 0 {
		eff.ContextLen = DefaultContextLen
		sources["context_length"] = "default"
	}
	if eff.GPUCount == 0 {
		eff.GPUCount = 1
	}

	return eff, sources
}

// applyPrimarySlot folds the first GPU position onto the profile, because the fit
// model scores against one card. The other positions are reported as slots but
// mixed cards are not combined into one virtual device.
func applyPrimarySlot(eff *smartdl.Hardware, sources map[string]string, slots []GPUSlot) {
	if len(slots) == 0 {
		return
	}
	primary := slots[0]
	eff.GPUCount = len(slots)
	if primary.Name == "" {
		return
	}
	eff.GPUName = primary.Name
	sources["gpu_name"] = primary.NameSource
	if primary.VRAMGB > 0 {
		eff.VRAMGB = primary.VRAMGB
		sources["vram_gb"] = primary.VRAMSource
	}
	if primary.BandwidthGBs > 0 {
		eff.BandwidthGBs = primary.BandwidthGBs
		sources["bandwidth_gbps"] = primary.BandwidthSource
	}
	if primary.Cores > 0 {
		sources["cores"] = primary.CoresSource
	}
}

// effectiveHardware runs one detection pass and merges the overrides on top.
func (s *Server) effectiveHardware(ctx context.Context) (smartdl.Hardware, map[string]string, float64, []string) {
	reading := s.detectHardware(ctx, devices.Basic)
	eff, sources := mergeHardware(reading.Detected, s.snapshotConfig().Hardware)
	applyPrimarySlot(&eff, sources, hardwareSlots(reading.DetectedGPUs, s.snapshotConfig().Hardware))
	return eff, sources, reading.VRAMUsedGB, reading.Gaps
}

// handleHardware returns the machine profile used for recommendations.
// ?fresh=1 drops the engine's static cache so the next read re-queries the
// devices; without it the cached snapshot is reused.
func (s *Server) handleHardware(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("fresh") == "1" {
		s.hardwareEngine().InvalidateStatic()
	}
	reading := s.detectHardware(r.Context(), devices.Basic)
	slots := hardwareSlots(reading.DetectedGPUs, s.snapshotConfig().Hardware)
	eff, sources := mergeHardware(reading.Detected, s.snapshotConfig().Hardware)
	applyPrimarySlot(&eff, sources, slots)
	cores := 0
	if len(slots) > 0 {
		cores = slots[0].Cores
	}

	resp := map[string]any{
		"effective":       eff,
		"sources":         sources,
		"gaps":            reading.Gaps,
		"catalog":         smartdl.CatalogNames(),
		"catalog_entries": smartdl.CatalogEntries(),
		"detected_gpus":   reading.DetectedGPUs,
		"gpus":            slots,
		"gpu_cores":       cores,
	}
	if reading.VRAMUsedGB > 0 {
		resp["vram_used_gb"] = reading.VRAMUsedGB
		if eff.VRAMGB > 0 {
			free := eff.VRAMGB - reading.VRAMUsedGB
			if free < 0 {
				free = 0
			}
			resp["vram_free_gb"] = free
		}
	}
	writeJSON(w, http.StatusOK, resp)
}
