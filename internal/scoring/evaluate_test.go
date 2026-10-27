package scoring

import (
	"encoding/json"
	"errors"
	"flag"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/SakkijarvenPolka/tahrs-scheduler/internal/model"
)

const gib = int64(1) << 30

var update = flag.Bool("update", false, "rewrite testdata/golden_evaluate.json")

var (
	profileCPUGeneral = model.ProfileView{
		Name:            "cpu-general-v1",
		Alpha:           0.3,
		Beta:            0.7,
		FitWeights:      model.Weights{CPU: 1, Memory: 1},
		PackWeights:     model.Weights{CPU: 1, Memory: 1},
		GPUPolicy:       model.GPUPolicyAvoid,
		GPUAvoidPenalty: 30,
	}
	profileGPUInference = model.ProfileView{
		Name:            "gpu-inference-v1",
		Alpha:           0.3,
		Beta:            0.7,
		FitWeights:      model.Weights{CPU: 1, Memory: 1, GPU: 3},
		PackWeights:     model.Weights{CPU: 1, Memory: 1, GPU: 2},
		GPUPolicy:       model.GPUPolicyAllow,
		GPUAvoidPenalty: 30,
	}
)

func res(cpuM int64, memGi float64, gpu int64) model.Resources {
	return model.Resources{MilliCPU: cpuM, MemoryBytes: int64(memGi * float64(gib)), GPUs: gpu}
}

func node(name string, alloc, used model.Resources) model.NodeSnapshot {
	return model.NodeSnapshot{Name: name, Allocatable: alloc, Used: used}
}

// evalCase is one row of the table driven suite. The four snapshot archetypes
// required by the plan (empty, half full, tight, single dimension exhausted)
// are exercised across several Pod shapes.
type evalCase struct {
	request model.TaskRequests
	node    model.NodeSnapshot
	profile model.ProfileView
}

// tableCases returns at least 20 cases covering the required archetypes.
func tableCases() map[string]evalCase {
	nodes := map[string]model.NodeSnapshot{
		// empty node
		"empty": node("node-empty", res(4000, 8, 0), res(0, 0, 0)),
		// half full
		"half": node("node-half", res(4000, 8, 0), res(2000, 4, 0)),
		// tight: a pod of the "medium" shape nearly fills it
		"tight": node("node-tight", res(4000, 8, 0), res(3500, 7, 0)),
		// single dimension exhausted: CPU is (almost) gone, memory is free
		"cpu-exhausted": node("node-cpu-exhausted", res(4000, 8, 0), res(3900, 1, 0)),
		// single dimension exhausted on a GPU node: cards are all taken
		"gpu-exhausted": node("node-gpu-exhausted", res(8000, 32, 4), res(4000, 16, 4)),
		// GPU node, empty
		"gpu-empty": node("node-gpu-empty", res(8000, 32, 4), res(0, 0, 0)),
		// GPU node, half full
		"gpu-half": node("node-gpu-half", res(8000, 32, 4), res(4000, 16, 2)),
		// over-committed node: Remaining must be clamped instead of going negative
		"overcommitted": node("node-overcommitted", res(1000, 2, 0), res(3000, 4, 0)),
	}
	pods := map[string]model.TaskRequests{
		"zero":      res(0, 0, 0),
		"tiny":      res(100, 0.125, 0),
		"small":     res(500, 0.5, 0),
		"medium":    res(2000, 4, 0),
		"mem-heavy": res(100, 6, 0),
		"cpu-only":  res(1000, 0, 0),
		"gpu1":      res(4000, 8, 1),
		"gpu3":      res(8000, 16, 3),
	}

	// The cross product of a subset of node/pod pairs keeps the table readable
	// while still covering every archetype with more than one Pod shape.
	pairs := []struct{ node, pod string }{
		{"empty", "zero"}, {"empty", "tiny"}, {"empty", "small"}, {"empty", "medium"},
		{"empty", "mem-heavy"}, {"empty", "cpu-only"}, {"empty", "gpu1"},
		{"half", "tiny"}, {"half", "small"}, {"half", "medium"}, {"half", "gpu1"},
		{"tight", "small"}, {"tight", "medium"}, {"tight", "cpu-only"},
		{"cpu-exhausted", "small"}, {"cpu-exhausted", "mem-heavy"}, {"cpu-exhausted", "cpu-only"},
		{"gpu-exhausted", "gpu1"}, {"gpu-exhausted", "gpu3"}, {"gpu-exhausted", "small"},
		{"gpu-empty", "gpu1"}, {"gpu-empty", "gpu3"}, {"gpu-empty", "small"}, {"gpu-half", "gpu1"},
		{"gpu-half", "gpu3"}, {"gpu-half", "medium"}, {"gpu-half", "zero"},
		{"overcommitted", "tiny"}, {"overcommitted", "small"},
	}

	cases := make(map[string]evalCase, len(pairs)*2)
	for _, pair := range pairs {
		n, ok := nodes[pair.node]
		if !ok {
			panic("unknown node " + pair.node)
		}
		q, ok := pods[pair.pod]
		if !ok {
			panic("unknown pod " + pair.pod)
		}
		// A Pod requesting a GPU on a node without GPUs is an anomaly, not a
		// scoring case; it is covered by TestPackZeroAllocatableIsAnomaly.
		if q.GPUs > n.Allocatable.GPUs {
			continue
		}
		cases["cpu-general/"+pair.node+"/"+pair.pod] = evalCase{request: q, node: n, profile: profileCPUGeneral}
		// GPU pods are only meaningful on GPU nodes; the CPU profile is still
		// exercised there because a GPU profile with a CPU-only Pod is the
		// interesting penalty case.
		if pair.node == "gpu-empty" || pair.node == "gpu-half" || pair.node == "gpu-exhausted" {
			cases["gpu-inference/"+pair.node+"/"+pair.pod] = evalCase{request: q, node: n, profile: profileGPUInference}
		}
	}
	return cases
}

type goldenEntry struct {
	Fit         float64 `json:"fit"`
	FitSkipped  bool    `json:"fitSkipped"`
	Pack        float64 `json:"pack"`
	PackSkipped bool    `json:"packSkipped"`
	Combined    float64 `json:"combined"`
	PrePenalty  int64   `json:"prePenalty"`
	GPUPenalty  int64   `json:"gpuPenalty"`
	Final       int64   `json:"final"`
}

func goldenPath() string { return filepath.Join("testdata", "golden_evaluate.json") }

func TestEvaluateGolden(t *testing.T) {
	cases := tableCases()
	if len(cases) < 20 {
		t.Fatalf("table has %d cases, at least 20 are required", len(cases))
	}

	got := make(map[string]goldenEntry, len(cases))
	names := make([]string, 0, len(cases))
	for name, c := range cases {
		bd, err := Evaluate(c.request, c.node, c.profile)
		if err != nil {
			t.Fatalf("%s: Evaluate returned error: %v", name, err)
		}
		if bd.Final < MinScore || bd.Final > MaxScore {
			t.Fatalf("%s: final score %d out of range [%d,%d]", name, bd.Final, MinScore, MaxScore)
		}
		got[name] = goldenEntry{
			Fit:         bd.Fit,
			FitSkipped:  bd.FitSkipped,
			Pack:        bd.Pack,
			PackSkipped: bd.PackSkipped,
			Combined:    bd.Combined,
			PrePenalty:  bd.PrePenalty,
			GPUPenalty:  bd.GPUPenalty,
			Final:       bd.Final,
		}
		names = append(names, name)
	}
	sort.Strings(names)

	if *update {
		if err := os.MkdirAll(filepath.Dir(goldenPath()), 0o755); err != nil {
			t.Fatalf("create testdata: %v", err)
		}
		encoded, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatalf("marshal golden: %v", err)
		}
		if err := os.WriteFile(goldenPath(), append(encoded, '\n'), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("golden file rewritten with %d cases", len(got))
		return
	}

	raw, err := os.ReadFile(goldenPath())
	if err != nil {
		t.Fatalf("read golden (run `go test ./internal/scoring -run TestEvaluateGolden -update` to create it): %v", err)
	}
	var want map[string]goldenEntry
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatalf("parse golden: %v", err)
	}

	for _, name := range names {
		w, ok := want[name]
		if !ok {
			t.Errorf("%s: missing from golden file, rerun with -update", name)
			continue
		}
		g := got[name]
		if !almostEqual(g.Fit, w.Fit) {
			t.Errorf("%s: fit = %.12f, want %.12f", name, g.Fit, w.Fit)
		}
		if !almostEqual(g.Pack, w.Pack) {
			t.Errorf("%s: pack = %.12f, want %.12f", name, g.Pack, w.Pack)
		}
		if !almostEqual(g.Combined, w.Combined) {
			t.Errorf("%s: combined = %.12f, want %.12f", name, g.Combined, w.Combined)
		}
		if g.FitSkipped != w.FitSkipped {
			t.Errorf("%s: fitSkipped = %v, want %v", name, g.FitSkipped, w.FitSkipped)
		}
		if g.PackSkipped != w.PackSkipped {
			t.Errorf("%s: packSkipped = %v, want %v", name, g.PackSkipped, w.PackSkipped)
		}
		if g.PrePenalty != w.PrePenalty || g.GPUPenalty != w.GPUPenalty || g.Final != w.Final {
			t.Errorf("%s: score = (pre %d, penalty %d, final %d), want (pre %d, penalty %d, final %d)",
				name, g.PrePenalty, g.GPUPenalty, g.Final, w.PrePenalty, w.GPUPenalty, w.Final)
		}
	}
	if len(want) != len(got) {
		t.Errorf("golden file has %d cases, table has %d; rerun with -update", len(want), len(got))
	}
}

// TestEvaluateGoldenSpotChecks pins a handful of values that were derived by
// hand, so a silently regenerated golden file cannot hide a formula change.
func TestEvaluateGoldenSpotChecks(t *testing.T) {
	// Empty node, 2 cores / 4Gi request, no usage. Remaining == allocatable.
	//   fit: t=(2,4) f=(4,8) -> 1-(2+4)/(6+12) = 1-6/18 = 0.666666...
	//   pack: u'=(0.5,0.5) -> 1-sqrt((0.25+0.25)/2) = 1-0.5 = 0.5
	//   combined: 0.3*0.6667+0.7*0.5 = 0.55 -> round(55) = 55, penalty 0 (no GPU)
	bd, err := Evaluate(res(2000, 4, 0), node("n", res(4000, 8, 0), res(0, 0, 0)), profileCPUGeneral)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertFloat(t, "fit", bd.Fit, 2.0/3.0)
	assertFloat(t, "pack", bd.Pack, 0.5)
	assertFloat(t, "combined", bd.Combined, 0.55)
	if bd.Final != 55 {
		t.Errorf("final = %d, want 55", bd.Final)
	}

	// A CPU-only Pod on an empty GPU node pays the avoid penalty:
	//   combined = 0.3*fit+0.7*pack, minus 30, floored at 0.
	q := res(1000, 0, 0)
	snap := node("gpunode", res(8000, 32, 4), res(0, 0, 0))
	bd, err = Evaluate(q, snap, profileCPUGeneral)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bd.GPUPenalty != 30 {
		t.Fatalf("gpu penalty = %d, want 30", bd.GPUPenalty)
	}
	wantFinal := bd.PrePenalty - 30
	if wantFinal < MinScore {
		wantFinal = MinScore
	}
	if bd.Final != wantFinal {
		t.Errorf("final = %d, want max(0, prePenalty(%d)-30) = %d", bd.Final, bd.PrePenalty, wantFinal)
	}

	// Same node, same Pod, but the profile allows GPU nodes: no penalty.
	bdAllow, err := Evaluate(q, snap, profileGPUInference)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bdAllow.GPUPenalty != 0 {
		t.Errorf("gpu penalty with policy allow = %d, want 0", bdAllow.GPUPenalty)
	}
	// The GPU profile weights the GPU dimension in both components, so its
	// components are not expected to match the CPU profile; only the penalty
	// policy difference is asserted here.
	if bdAllow.PrePenalty < 0 || bdAllow.PrePenalty > MaxScore {
		t.Errorf("prePenalty %d out of range", bdAllow.PrePenalty)
	}
}

func TestFitSkipsZeroRequestDimensions(t *testing.T) {
	w := model.Weights{CPU: 1, Memory: 1, GPU: 1}
	snap := node("n", res(4000, 8, 2), res(0, 0, 0))

	// Nothing requested at all: every dimension is skipped, neutral value.
	fit, used := Fit(res(0, 0, 0), snap, w)
	if used {
		t.Fatalf("fit reported used=true for an empty request")
	}
	assertFloat(t, "neutral fit", fit, Neutral)

	// Zero weight for every dimension is also "nothing to evaluate".
	fit, used = Fit(res(500, 0.5, 1), snap, model.Weights{})
	if used {
		t.Fatalf("fit reported used=true with zero weights")
	}
	assertFloat(t, "neutral fit", fit, Neutral)

	// Only the memory dimension has a request: the result must equal the
	// single-dimension Bray-Curtis value, proving the other dims were skipped.
	// remaining 8Gi, request 2Gi -> f=8, t=2 -> 1-6/10 = 0.4
	fit, used = Fit(res(0, 2, 0), snap, w)
	if !used {
		t.Fatalf("fit reported used=false for a memory-only request")
	}
	assertFloat(t, "memory-only fit", fit, 0.4)

	// A dimension with a weight of zero must not contribute.
	fitSingle, used := Fit(res(1000, 2, 0), snap, model.Weights{Memory: 1})
	if !used {
		t.Fatalf("fit reported used=false")
	}
	fitMemoryOnly, _ := Fit(res(0, 2, 0), snap, model.Weights{Memory: 1})
	assertFloat(t, "zero weight dimension contributes", fitSingle, fitMemoryOnly)
}

func TestZeroRequestIsNeutral(t *testing.T) {
	// The plan requires a neutral 0.5 for a zero request. Fit is the component
	// that has no opinion in that case; Pack still has one, because packing a
	// zero-request Pod still says something about the node's occupancy.
	q := res(0, 0, 0)
	snap := node("n", res(4000, 8, 0), res(0, 0, 0))
	bd, err := Evaluate(q, snap, model.ProfileView{
		Name:        "fit-only",
		Alpha:       1,
		Beta:        0,
		FitWeights:  model.Weights{CPU: 1, Memory: 1},
		PackWeights: model.Weights{CPU: 1, Memory: 1},
		GPUPolicy:   model.GPUPolicyAllow,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bd.FitSkipped {
		t.Errorf("FitSkipped = false, want true for a zero request")
	}
	assertFloat(t, "fit", bd.Fit, Neutral)
	if bd.Final != 50 {
		t.Errorf("final = %d, want 50 (neutral 0.5 rounded)", bd.Final)
	}
}

func TestPackZeroAllocatableIsAnomaly(t *testing.T) {
	// A Pod requesting a GPU on a node that advertises none is an anomaly: a
	// Filter plugin should have rejected the node, so Pack reports an error and
	// Evaluate surfaces it instead of producing a meaningless score.
	_, _, err := Pack(res(1000, 1, 1), node("cpuonly", res(4000, 8, 0), res(0, 0, 0)),
		model.Weights{CPU: 1, Memory: 1, GPU: 1})
	if !errors.Is(err, model.ErrGPUNodeInsufficient) {
		t.Fatalf("pack error = %v, want ErrGPUNodeInsufficient", err)
	}
	if _, err := Evaluate(res(1000, 1, 1), node("cpuonly", res(4000, 8, 0), res(0, 0, 0)),
		profileGPUInference); !errors.Is(err, model.ErrGPUNodeInsufficient) {
		t.Fatalf("evaluate error = %v, want ErrGPUNodeInsufficient", err)
	}

	// Zero GPU capacity is fine when the Pod does not request a GPU.
	if _, _, err := Pack(res(1000, 1, 0), node("cpuonly", res(4000, 8, 0), res(0, 0, 0)),
		model.Weights{CPU: 1, Memory: 1, GPU: 1}); err != nil {
		t.Fatalf("unexpected error for a non-GPU Pod: %v", err)
	}

	// All pack weights zero means nothing to evaluate: neutral, no error.
	v, used, err := Pack(res(1000, 1, 0), node("n", res(4000, 8, 0), res(0, 0, 0)), model.Weights{})
	if err != nil || used {
		t.Fatalf("pack with zero weights: used=%v err=%v, want used=false err=nil", used, err)
	}
	assertFloat(t, "neutral pack", v, Neutral)
}

func TestEvaluateRejectsMalformedProfile(t *testing.T) {
	q := res(500, 0.5, 0)
	snap := node("n", res(4000, 8, 0), res(0, 0, 0))

	base := profileCPUGeneral
	broken := []struct {
		name    string
		mutate  func(*model.ProfileView)
		wantSub string
	}{
		{"negative alpha", func(p *model.ProfileView) { p.Alpha = -1 }, "alpha is negative"},
		{"negative beta", func(p *model.ProfileView) { p.Beta = -1 }, "beta is negative"},
		{"zero weights", func(p *model.ProfileView) { p.Alpha, p.Beta = 0, 0 }, "both zero"},
		{"negative fit weight", func(p *model.ProfileView) { p.FitWeights.CPU = -1 }, "negative"},
		{"unknown policy", func(p *model.ProfileView) { p.GPUPolicy = "sometimes" }, "unknown gpu node policy"},
		{"penalty out of range", func(p *model.ProfileView) { p.GPUAvoidPenalty = 101 }, "out of range"},
	}
	for _, tc := range broken {
		p := base
		tc.mutate(&p)
		if _, err := Evaluate(q, snap, p); err == nil {
			t.Errorf("%s: expected an error, got nil", tc.name)
		} else if !strings.Contains(err.Error(), tc.wantSub) {
			t.Errorf("%s: error %q does not mention %q", tc.name, err, tc.wantSub)
		}
	}
}

// TestScoreCompositionOrder pins clamp -> round -> penalty -> floor.
func TestScoreCompositionOrder(t *testing.T) {
	cases := []struct {
		combined float64
		penalty  int64
		want     int64
		why      string
	}{
		{0.999, 30, 70, "99.9 rounds up to 100 before the penalty is applied"},
		{0.25, 30, 0, "25-30 is floored at MinScore"},
		{1.0, 0, 100, "maximum"},
		{0.0, 0, 0, "minimum"},
		{1.7, 0, 100, "clamped before rounding"},
		{-0.4, 0, 0, "clamped before rounding"},
		{0.994, 0, 99, "99.4 rounds down"},
		{0.996, 0, 100, "99.6 rounds up"},
	}
	for _, tc := range cases {
		if got := scoreFromComponents(tc.combined, tc.penalty); got != tc.want {
			t.Errorf("scoreFromComponents(%v, %d) = %d, want %d (%s)", tc.combined, tc.penalty, got, tc.want, tc.why)
		}
	}

	// alpha+beta > 1 must be clamped, not overflow into a score above 100.
	p := profileGPUInference
	p.Alpha, p.Beta = 1, 1
	bd, err := Evaluate(res(1, 0.01, 0), node("n", res(1000, 1, 0), res(0, 0, 0)), p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bd.Combined > 1 {
		t.Fatalf("combined %v not clamped", bd.Combined)
	}
	if bd.Final > MaxScore {
		t.Fatalf("final %d exceeds MaxScore", bd.Final)
	}
}

// TestDeterministicBits requires bit-identical results for identical input.
// The implementation iterates model.Dimensions (a fixed slice) and never a map.
func TestDeterministicBits(t *testing.T) {
	first, err := Evaluate(res(500, 0.5, 0), node("n", res(4000, 8, 0), res(1000, 2, 0)), profileGPUInference)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for i := 0; i < 2000; i++ {
		next, err := Evaluate(res(500, 0.5, 0), node("n", res(4000, 8, 0), res(1000, 2, 0)), profileGPUInference)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if math.Float64bits(next.Fit) != math.Float64bits(first.Fit) ||
			math.Float64bits(next.Pack) != math.Float64bits(first.Pack) ||
			math.Float64bits(next.Combined) != math.Float64bits(first.Combined) ||
			next.Final != first.Final {
			t.Fatalf("iteration %d produced a different result: %+v vs %+v", i, next, first)
		}
	}
}

// TestFitAndPackDisagree is the F12 evidence case: the two components order the
// same two nodes in opposite directions, so the composite score really is a
// trade-off and not a duplicated signal.
//
//	node A: 4000m / 4Gi, idle  -> after placement u'=(0.125, 0.25)
//	node B: 600m / 16Gi, idle  -> after placement u'=(0.833, 0.0625)
//
// Fit prefers A (its relative residual mismatch is smaller), Pack prefers B
// (it looks fuller in allocatable-relative terms). A is the "single dimension
// exhausted" node from the plan: it has plenty of everything relative to the
// request, B has its CPU almost exhausted by the very same Pod.
func TestFitAndPackDisagree(t *testing.T) {
	q := res(500, 1, 0)
	a := node("a-small-balanced", res(4000, 4, 0), res(0, 0, 0))
	b := node("b-cpu-tight", res(600, 16, 0), res(0, 0, 0))
	w := model.Weights{CPU: 1, Memory: 1}

	fitA, _ := Fit(q, a, w)
	fitB, _ := Fit(q, b, w)
	packA, _, err := Pack(q, a, w)
	if err != nil {
		t.Fatalf("pack A: %v", err)
	}
	packB, _, err := Pack(q, b, w)
	if err != nil {
		t.Fatalf("pack B: %v", err)
	}

	if !(fitA > fitB) {
		t.Errorf("expected fit(A)=%v > fit(B)=%v", fitA, fitB)
	}
	if !(packA < packB) {
		t.Errorf("expected pack(A)=%v < pack(B)=%v", packA, packB)
	}

	// The composite ranking therefore flips with the weights, which is exactly
	// what the profile parameters are for.
	fitHeavy := model.ProfileView{
		Name: "fit-heavy", Alpha: 1, Beta: 0,
		FitWeights: w, PackWeights: w, GPUPolicy: model.GPUPolicyAllow,
	}
	packHeavy := model.ProfileView{
		Name: "pack-heavy", Alpha: 0, Beta: 1,
		FitWeights: w, PackWeights: w, GPUPolicy: model.GPUPolicyAllow,
	}
	bdA, err := Evaluate(q, a, fitHeavy)
	if err != nil {
		t.Fatalf("evaluate A: %v", err)
	}
	bdB, err := Evaluate(q, b, fitHeavy)
	if err != nil {
		t.Fatalf("evaluate B: %v", err)
	}
	if bdA.Final <= bdB.Final {
		t.Errorf("fit-heavy ranking: A=%d B=%d, want A>B", bdA.Final, bdB.Final)
	}
	bdA, err = Evaluate(q, a, packHeavy)
	if err != nil {
		t.Fatalf("evaluate A: %v", err)
	}
	bdB, err = Evaluate(q, b, packHeavy)
	if err != nil {
		t.Fatalf("evaluate B: %v", err)
	}
	if bdA.Final >= bdB.Final {
		t.Errorf("pack-heavy ranking: A=%d B=%d, want A<B", bdA.Final, bdB.Final)
	}
}

func TestOvercommittedNodeStaysInRange(t *testing.T) {
	// used > allocatable happens when another scheduler bound pods or requests
	// were scaled. Remaining is clamped, so both components stay in [0,1].
	snap := node("over", res(1000, 2, 0), res(3000, 6, 0))
	bd, err := Evaluate(res(500, 1, 0), snap, profileCPUGeneral)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bd.Fit < 0 || bd.Fit > 1 || bd.Pack < 0 || bd.Pack > 1 {
		t.Fatalf("components out of range: fit=%v pack=%v", bd.Fit, bd.Pack)
	}
	rem := snap.Remaining()
	if rem.MilliCPU != 0 || rem.MemoryBytes != 0 {
		t.Fatalf("remaining = %+v, want zeroes", rem)
	}
}

func TestGPUPenaltySemantics(t *testing.T) {
	gpuNode := node("gpu", res(4000, 8, 2), res(0, 0, 0))

	if p, ok := GPUPenalty(res(500, 0.5, 0), gpuNode, profileCPUGeneral); !ok || p != 30 {
		t.Errorf("CPU pod on GPU node: penalty=%d ok=%v, want 30/true", p, ok)
	}
	if p, ok := GPUPenalty(res(500, 0.5, 1), gpuNode, profileCPUGeneral); ok || p != 0 {
		t.Errorf("GPU pod: penalty=%d ok=%v, want 0/false", p, ok)
	}
	if p, ok := GPUPenalty(res(500, 0.5, 0), gpuNode, profileGPUInference); ok || p != 0 {
		t.Errorf("policy allow: penalty=%d ok=%v, want 0/false", p, ok)
	}
	if p, ok := GPUPenalty(res(500, 0.5, 0), node("cpu", res(4000, 8, 0), res(0, 0, 0)), profileCPUGeneral); ok || p != 0 {
		t.Errorf("CPU node: penalty=%d ok=%v, want 0/false", p, ok)
	}
}

func almostEqual(a, b float64) bool {
	return math.Abs(a-b) <= 1e-9
}

func assertFloat(t *testing.T, name string, got, want float64) {
	t.Helper()
	if !almostEqual(got, want) {
		t.Errorf("%s = %.12f, want %.12f", name, got, want)
	}
}
