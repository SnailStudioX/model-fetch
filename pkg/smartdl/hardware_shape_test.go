// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package smartdl

import "testing"

// TestModelShapeFromInfoGGUFWithoutConfigJSON pins the parameter count for a
// GGUF-only repo. Those usually carry no config.json, and ModelShapeFromInfo
// stopped at that missing key, so ParamsBillions stayed zero and the
// GGUF branch of ParamsBillions was unreachable: every quant scored without the
// parameter-count bonus, and a Mixture-of-Experts GGUF (where the file is far
// larger than the per-token working set) could never get its speed estimate
// scaled by the active/total ratio.
func TestModelShapeFromInfoGGUFWithoutConfigJSON(t *testing.T) {
	info := &RepoInfo{
		Type: TypeGGUF,
		GGUF: &GGUFInfo{ParameterCount: "30B"},
	}

	shape := ModelShapeFromInfo(info)
	if shape.ParamsBillions != 30 {
		t.Errorf("ParamsBillions = %v, want 30 from the GGUF metadata", shape.ParamsBillions)
	}
	if shape.Layers != 0 || shape.KVHeads != 0 || shape.HeadDim != 0 {
		t.Errorf("invented attention geometry for a repo that declares none: %+v", shape)
	}
	if shape.ActiveParamsBillions != 0 {
		t.Errorf("ActiveParamsBillions = %v without a config.json to derive it from", shape.ActiveParamsBillions)
	}
}
