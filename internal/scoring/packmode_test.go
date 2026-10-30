package scoring

import (
	"math"
	"strings"
	"testing"

	"github.com/SakkijarvenPolka/tahrs-scheduler/internal/model"
)

func packModeProfile(mode model.PackMode) model.ProfileView {
	return model.ProfileView{
		Name:        "pack-mode",
		Alpha:       0.3,
		Beta:        0.7,
		FitWeights:  model.Weights{CPU: 1},
		PackWeights: model.Weights{CPU: 1},
		PackMode:    mode,
		GPUPolicy:   model.GPUPolicyAllow,
	}
}

// TestPackModeReversesDirection pins the only behavioural difference between the
// two modes: for identical inputs the fuller node wins in pack mode and the
// emptier node wins in spread mode.
func TestPackModeReversesDirection(t *testing.T) {
	request := model.TaskRequests{MilliCPU: 1000}
	empty := model.NodeSnapshot{Name: "empty", Allocatable: model.Resources{MilliCPU: 8000}}
	full := model.NodeSnapshot{
		Name:        "full",
		Allocatable: model.Resources{MilliCPU: 8000},
		Used:        model.Resources{MilliCPU: 6000},
	}

	packEmpty, err := Evaluate(request, empty, packModeProfile(model.PackModePack))
	if err != nil {
		t.Fatalf("pack/empty: %v", err)
	}
	packFull, err := Evaluate(request, full, packModeProfile(model.PackModePack))
	if err != nil {
		t.Fatalf("pack/full: %v", err)
	}
	if packFull.Final <= packEmpty.Final {
		t.Errorf("pack mode should prefer the fuller node: empty=%d full=%d", packEmpty.Final, packFull.Final)
	}

	spreadEmpty, err := Evaluate(request, empty, packModeProfile(model.PackModeSpread))
	if err != nil {
		t.Fatalf("spread/empty: %v", err)
	}
	spreadFull, err := Evaluate(request, full, packModeProfile(model.PackModeSpread))
	if err != nil {
		t.Fatalf("spread/full: %v", err)
	}
	if spreadEmpty.Final <= spreadFull.Final {
		t.Errorf("spread mode should prefer the emptier node: empty=%d full=%d", spreadEmpty.Final, spreadFull.Final)
	}

	// The two modes must be exact complements of each other, otherwise the A/B
	// is not measuring the direction alone.
	if math.Abs(spreadEmpty.Pack+packEmpty.Pack-1) > 1e-9 {
		t.Errorf("empty node pack values are not complementary: pack=%v spread=%v", packEmpty.Pack, spreadEmpty.Pack)
	}
	if spreadEmpty.PackMode != model.PackModeSpread || packEmpty.PackMode != model.PackModePack {
		t.Errorf("breakdown pack mode not recorded: %q / %q", packEmpty.PackMode, spreadEmpty.PackMode)
	}
}

// TestPackModeKeepsNeutralSymmetric covers the degraded path: a profile whose
// only pack dimension is skipped uses the neutral value, which must not change
// direction.
func TestPackModeKeepsNeutralSymmetric(t *testing.T) {
	profile := func(mode model.PackMode) model.ProfileView {
		return model.ProfileView{
			Name:        "neutral",
			Alpha:       0.3,
			Beta:        0.7,
			FitWeights:  model.Weights{CPU: 1},
			PackWeights: model.Weights{GPU: 1},
			PackMode:    mode,
			GPUPolicy:   model.GPUPolicyAllow,
		}
	}
	// CPU only node, Pod without a GPU request: the GPU pack dimension is skipped.
	node := model.NodeSnapshot{Name: "n", Allocatable: model.Resources{MilliCPU: 4000, MemoryBytes: 8 << 30}}
	packed, err := Evaluate(model.TaskRequests{MilliCPU: 500}, node, profile(model.PackModePack))
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	spread, err := Evaluate(model.TaskRequests{MilliCPU: 500}, node, profile(model.PackModeSpread))
	if err != nil {
		t.Fatalf("spread: %v", err)
	}
	if !packed.PackSkipped || !spread.PackSkipped {
		t.Fatalf("expected the pack component to be skipped: pack=%v spread=%v", packed.PackSkipped, spread.PackSkipped)
	}
	if packed.Final != spread.Final {
		t.Errorf("neutral value changed with the mode: pack=%d spread=%d", packed.Final, spread.Final)
	}
}

func TestEvaluateRejectsUnknownPackMode(t *testing.T) {
	_, err := Evaluate(model.TaskRequests{MilliCPU: 1000},
		model.NodeSnapshot{Name: "n", Allocatable: model.Resources{MilliCPU: 4000}},
		packModeProfile(model.PackMode("sideways")))
	if err == nil {
		t.Fatal("expected an error for an unknown pack mode")
	}
	if !strings.Contains(err.Error(), "pack mode") {
		t.Errorf("error should name the pack mode, got %q", err)
	}
}
