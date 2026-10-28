package scoring

import (
	"math"
	"strings"
	"testing"

	"github.com/SakkijarvenPolka/tahrs-scheduler/internal/model"
)

// finiteProfile is a profile that Evaluate accepts.
func finiteProfile() model.ProfileView {
	return model.ProfileView{
		Name:        "cpu-general-v1",
		Alpha:       0.3,
		Beta:        0.7,
		FitWeights:  model.Weights{CPU: 1, Memory: 1},
		PackWeights: model.Weights{CPU: 1, Memory: 1},
		GPUPolicy:   model.GPUPolicyAvoid,
	}
}

func sampleInputs() (model.TaskRequests, model.NodeSnapshot) {
	return model.TaskRequests{MilliCPU: 500, MemoryBytes: 1 << 30},
		model.NodeSnapshot{
			Name:        "node-a",
			Allocatable: model.Resources{MilliCPU: 4000, MemoryBytes: 8 << 30},
			Used:        model.Resources{MilliCPU: 500, MemoryBytes: 1 << 30},
		}
}

// TestEvaluateAcceptsFiniteProfile is the control: the same inputs score fine.
func TestEvaluateAcceptsFiniteProfile(t *testing.T) {
	q, snap := sampleInputs()
	if _, err := Evaluate(q, snap, finiteProfile()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
}

// TestEvaluateRejectsNonFiniteProfile pins the defect that made a NaN alpha
// return a perfect score: NaN < 0 and NaN > 1 are both false, so the clamp kept
// NaN, math.Round(NaN) converted to MinInt64, and the score was clamped up to
// MaxScore. Evaluate must now fail loudly instead.
func TestEvaluateRejectsNonFiniteProfile(t *testing.T) {
	q, snap := sampleInputs()

	cases := []struct {
		name   string
		mutate func(*model.ProfileView)
		want   string
	}{
		{name: "alpha NaN", mutate: func(p *model.ProfileView) { p.Alpha = math.NaN() }, want: "alpha is not finite"},
		{name: "beta +Inf", mutate: func(p *model.ProfileView) { p.Beta = math.Inf(1) }, want: "beta is not finite"},
		{name: "fit weight NaN", mutate: func(p *model.ProfileView) { p.FitWeights.Memory = math.NaN() }, want: "fit weight for \"memory\" is not finite"},
		{name: "pack weight -Inf", mutate: func(p *model.ProfileView) { p.PackWeights.CPU = math.Inf(-1) }, want: "pack weight for \"cpu\" is not finite"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			view := finiteProfile()
			tc.mutate(&view)

			breakdown, err := Evaluate(q, snap, view)
			if err == nil {
				t.Fatalf("Evaluate accepted a non-finite profile and scored %d", breakdown.Final)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err.Error(), tc.want)
			}
		})
	}
}
