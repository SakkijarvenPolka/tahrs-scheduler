package config

import (
	"testing"

	"github.com/SakkijarvenPolka/tahrs-scheduler/internal/model"
)

func TestCompileMapsDimensionsAndPolicy(t *testing.T) {
	loaded := mustLoadExample(t)
	views, err := loaded.Setting.Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if len(views) != 2 {
		t.Fatalf("views = %d, want 2", len(views))
	}

	cpu, ok := views["cpu-general-v1"]
	if !ok {
		t.Fatal("cpu-general-v1 missing from the compiled views")
	}
	if cpu.Alpha != 0.3 || cpu.Beta != 0.7 {
		t.Errorf("alpha/beta = %v/%v", cpu.Alpha, cpu.Beta)
	}
	if cpu.FitWeights != (model.Weights{CPU: 1, Memory: 1}) {
		t.Errorf("fit weights = %+v, want cpu=1 memory=1", cpu.FitWeights)
	}
	if cpu.PackWeights != (model.Weights{CPU: 1, Memory: 1}) {
		t.Errorf("pack weights = %+v", cpu.PackWeights)
	}
	if cpu.GPUPolicy != model.GPUPolicyAvoid {
		t.Errorf("policy = %q, want avoid", cpu.GPUPolicy)
	}
	if cpu.GPUAvoidPenalty != 30 {
		t.Errorf("penalty = %d, want the setting-level 30", cpu.GPUAvoidPenalty)
	}

	gpu := views["gpu-inference-v1"]
	if gpu.FitWeights.GPU != 3 || gpu.PackWeights.GPU != 2 {
		t.Errorf("gpu weights = fit %v pack %v, want 3 and 2", gpu.FitWeights.GPU, gpu.PackWeights.GPU)
	}
	if gpu.GPUPolicy != model.GPUPolicyAllow {
		t.Errorf("gpu policy = %q, want allow", gpu.GPUPolicy)
	}
}

func TestCompiledViewsPassModelValidation(t *testing.T) {
	loaded := mustLoadExample(t)
	views, err := loaded.Setting.Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	for name, view := range views {
		if err := view.Validate(); err != nil {
			t.Errorf("profile %q: %v", name, err)
		}
	}
}

func TestCompileRejectsUnknownDimension(t *testing.T) {
	setting := Setting{
		ConfigVersion:   "v1",
		GPUResourceName: "nvidia.com/gpu",
		DefaultProfile:  "p",
		Profiles: map[string]Profile{
			"p": {Alpha: 0.3, Beta: 0.7, FitWeights: map[string]float64{"disk": 1}},
		},
	}
	if _, err := setting.Compile(); err == nil {
		t.Fatal("Compile accepted an unknown dimension")
	}
}

func TestProfileNamesSorted(t *testing.T) {
	views := map[string]model.ProfileView{
		"zeta":  {},
		"alpha": {},
		"mid":   {},
	}
	got := ProfileNames(views)
	want := []string{"alpha", "mid", "zeta"}
	if len(got) != len(want) {
		t.Fatalf("names = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("names = %v, want %v", got, want)
		}
	}
}
