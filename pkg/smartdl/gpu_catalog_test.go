package smartdl

import "testing"

// The API exposes the catalog to the UI, so the exposed values must agree with
// what the recommendation path resolves.
func TestCatalogEntriesMatchLookup(t *testing.T) {
	entries := CatalogEntries()
	if len(entries) < 50 {
		t.Fatalf("catalog too small: %d entries", len(entries))
	}
	if e := entries["rtx 3090"]; e.VRAMGB != 24 || e.BandwidthGBs != 936 {
		t.Errorf("catalog rtx 3090 = %+v, want 24 GB / 936 GB/s", e)
	}
	for name, e := range entries {
		if got := LookupVRAM(name); got != e.VRAMGB {
			t.Errorf("LookupVRAM(%q) = %v, catalog says %v", name, got, e.VRAMGB)
		}
		if got := LookupBandwidth(name); got != e.BandwidthGBs {
			t.Errorf("LookupBandwidth(%q) = %v, catalog says %v", name, got, e.BandwidthGBs)
		}
	}
}

// Two entries were corrected against official specs; keep them pinned so a
// future re-sync does not silently reintroduce the wrong numbers.
func TestCatalogSpecCorrections(t *testing.T) {
	if e := CatalogEntries()["t4"]; e.BandwidthGBs != 320 {
		t.Errorf("catalog t4 bandwidth = %v, want 320 GB/s (TechPowerUp)", e.BandwidthGBs)
	}
	if e := CatalogEntries()["rx 9070"]; e.VRAMGB != 16 || e.BandwidthGBs != 640 {
		t.Errorf("catalog rx 9070 = %+v, want 16 GB / 640 GB/s", e)
	}
}
