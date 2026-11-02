// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package smartdl

import "testing"

// TestLookupCatalogMatchesAdapterWordOrder pins the separator handling. Adapter
// strings put the memory size behind "-", "_" or a bus suffix, while the catalog
// key writes it as a plain space: "a100 40gb" never appeared inside
// "NVIDIA A100-SXM4-40GB" as a substring, so only the bare "a100" row matched and
// a 55 GB model was reported as fitting a 40 GB card.
func TestLookupCatalogMatchesAdapterWordOrder(t *testing.T) {
	for _, tc := range []struct {
		adapter string
		vram    float64
		bw      float64
	}{
		{"NVIDIA A100-SXM4-40GB", 40, 1555},
		{"Tesla V100-SXM2-32GB", 32, 900},
		{"NVIDIA GeForce RTX 3050 6GB", 6, 168},
		// Still resolves the plain row when the name carries no memory word.
		{"NVIDIA A100-SXM4-80GB", 80, 2039},
	} {
		if got := LookupVRAM(tc.adapter); got != tc.vram {
			t.Errorf("LookupVRAM(%q) = %v, want %v", tc.adapter, got, tc.vram)
		}
		if got := LookupBandwidth(tc.adapter); got != tc.bw {
			t.Errorf("LookupBandwidth(%q) = %v, want %v", tc.adapter, got, tc.bw)
		}
	}
}

// TestSameDeviceNameKeepsSKUsApart pins that an override written for a different
// SKU is not folded onto the detected adapter. Substring equality said
// "RTX 3080" and "RTX 3080 Ti" were the same card, so the VRAM the user typed
// for the Ti was discarded as a duplicate.
func TestSameDeviceNameKeepsSKUsApart(t *testing.T) {
	for _, tc := range [][2]string{
		{"RTX 3090", "NVIDIA GeForce RTX 3090"},
		{"A100 40GB", "NVIDIA A100-SXM4-40GB"},
	} {
		if !SameDeviceName(tc[0], tc[1]) {
			t.Errorf("SameDeviceName(%q, %q) = false, want true: one card, two spellings", tc[0], tc[1])
		}
	}
	for _, tc := range [][2]string{
		{"RTX 3080", "RTX 3080 Ti"},
		{"A100", "NVIDIA A100-SXM4-40GB"},
		{"RTX 3050", "RTX 3050 6GB"},
		{"Tesla V100", "Tesla V100-SXM2-32GB"},
	} {
		if SameDeviceName(tc[0], tc[1]) {
			t.Errorf("SameDeviceName(%q, %q) = true, want false: different SKUs", tc[0], tc[1])
		}
	}
}
