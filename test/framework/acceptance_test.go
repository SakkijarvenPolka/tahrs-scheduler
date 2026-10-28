package framework_test

import (
	"fmt"
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	fwk "k8s.io/kube-scheduler/framework"
	st "k8s.io/kubernetes/pkg/scheduler/testing"

	"github.com/SakkijarvenPolka/tahrs-scheduler/internal/replay"
	"github.com/SakkijarvenPolka/tahrs-scheduler/internal/scoring"
)

// TestUnknownProfileRejectedInPreFilter is T3 criterion 1.
//
// PreFilter must reject the pod, not Score. With a single feasible node the
// framework skips Score entirely, so a check performed in Score would silently
// let an unknown profile through on small clusters.
func TestUnknownProfileRejectedInPreFilter(t *testing.T) {
	for _, nodeCount := range []int{1, 3} {
		t.Run(fmt.Sprintf("%d-node-cluster", nodeCount), func(t *testing.T) {
			nodes := make([]*v1.Node, 0, nodeCount)
			for i := range nodeCount {
				nodes = append(nodes, node(fmt.Sprintf("node-%d", i), "4000m", "8Gi", 0))
			}
			e := newEnv(t, cpuGeneralSetting(), nodes, false)
			requests := map[v1.ResourceName]string{
				v1.ResourceCPU:    "100m",
				v1.ResourceMemory: "64Mi",
			}

			res := e.run(pod("no-such-profile", requests))

			if got := statusCode(res.preFilter); got != fwk.UnschedulableAndUnresolvable {
				t.Fatalf("prefilter code = %v, want %v", got, fwk.UnschedulableAndUnresolvable)
			}
			message := res.preFilter.Message()
			for _, want := range []string{"no-such-profile", e.config.Hash, "cpu-general-v1"} {
				if !strings.Contains(message, want) {
					t.Errorf("prefilter message %q does not contain %q", message, want)
				}
			}
			if len(res.scores) != 0 {
				t.Fatalf("scored %d nodes despite prefilter rejection: %v", len(res.scores), res.scores)
			}

			// Control: the same cluster accepts the default profile, so the
			// rejection above is caused by the annotation, not the fixture.
			ok := e.run(pod("", requests))
			if got := statusCode(ok.preFilter); got != fwk.Success {
				t.Fatalf("default profile prefilter code = %v, want success (message %q)", got, ok.preFilter.Message())
			}
			if nodeCount == 1 {
				// A single feasible node is returned un-scored (upstream
				// schedule_one.go), so the control checks selection instead.
				if ok.selected != "node-0" {
					t.Fatalf("default profile selected %q, want node-0", ok.selected)
				}
			} else if len(ok.scores) != nodeCount {
				t.Fatalf("scored %d nodes, want %d", len(ok.scores), nodeCount)
			}
		})
	}
}

// TestCPUWorkloadNeverLandsOnTaintedGPUNode is T3 criterion 2.
func TestCPUWorkloadNeverLandsOnTaintedGPUNode(t *testing.T) {
	gpuTaint := v1.Taint{Key: "dedicated", Value: "tahrs", Effect: v1.TaintEffectNoSchedule}
	nodes := []*v1.Node{
		node("gpu-1", "8000m", "32Gi", 4, gpuTaint),
		node("cpu-1", "8000m", "32Gi", 0),
	}
	e := newEnv(t, cpuGeneralSetting(), nodes, true)

	res := e.run(pod("", map[v1.ResourceName]string{
		v1.ResourceCPU:    "500m",
		v1.ResourceMemory: "512Mi",
	}))

	if got := res.filterStatus["gpu-1"]; !isUnschedulable(got) {
		t.Fatalf("gpu-1 filter verdict = %v, want an unschedulable code (message %q)", got, res.filterMessage["gpu-1"])
	}
	if got := res.filterStatus["cpu-1"]; got != fwk.Success {
		t.Fatalf("cpu-1 filter verdict = %v, want success (message %q)", got, res.filterMessage["cpu-1"])
	}
	if res.selected != "cpu-1" {
		t.Fatalf("selected %q, want cpu-1: the only feasible node is returned without scoring", res.selected)
	}
}

// TestGPUWorkloadOrderingMatchesReplay is T3 criterion 3: the plugin running
// inside the framework must produce exactly the scores `tahrsctl replay` prints
// for the same cluster, so operators can tune a configuration offline.
func TestGPUWorkloadOrderingMatchesReplay(t *testing.T) {
	type fixture struct {
		name           string
		cpu, memory    string
		gpus, usedGPUs int64
	}
	fixtures := []fixture{
		// gpu-a is nearly exhausted: one free card.
		{name: "gpu-a", cpu: "8000m", memory: "32Gi", gpus: 2, usedGPUs: 1},
		// gpu-b is wide open.
		{name: "gpu-b", cpu: "32000m", memory: "128Gi", gpus: 8, usedGPUs: 0},
		// gpu-c has two of four cards left.
		{name: "gpu-c", cpu: "16000m", memory: "64Gi", gpus: 4, usedGPUs: 2},
		// cpu-d has no accelerators at all.
		{name: "cpu-d", cpu: "8000m", memory: "32Gi", gpus: 0, usedGPUs: 0},
	}

	nodes := make([]*v1.Node, 0, len(fixtures))
	resident := make([]*v1.Pod, 0, len(fixtures))
	cluster := replay.Cluster{Nodes: make([]replay.Node, 0, len(fixtures))}
	for _, f := range fixtures {
		nodes = append(nodes, node(f.name, f.cpu, f.memory, f.gpus))
		node := replay.Node{
			Name:        f.name,
			Allocatable: map[string]string{"cpu": f.cpu, "memory": f.memory},
		}
		if f.gpus > 0 {
			node.Allocatable[gpuResource] = fmt.Sprintf("%d", f.gpus)
		}
		if f.usedGPUs > 0 {
			node.Requested = map[string]string{gpuResource: fmt.Sprintf("%d", f.usedGPUs)}
			// The framework derives node usage from the pods bound to it, so the
			// fixture binds placeholder pods that already hold the cards.
			resident = append(resident, st.MakePod().
				Name("resident-"+f.name).
				Namespace("default").
				Node(f.name).
				Req(map[v1.ResourceName]string{v1.ResourceName(gpuResource): fmt.Sprintf("%d", f.usedGPUs)}).
				Obj())
		}
		cluster.Nodes = append(cluster.Nodes, node)
	}

	setting := gpuInferenceSetting()
	cfgPath, loaded := writeConfig(t, setting)

	workloadRequests := map[v1.ResourceName]string{
		v1.ResourceCPU:               "2000m",
		v1.ResourceMemory:            "4Gi",
		v1.ResourceName(gpuResource): "1",
	}
	workload := replay.Workload{
		Name:     "inference-1",
		Profile:  "gpu-inference-v1",
		Requests: map[string]string{"cpu": "2000m", "memory": "4Gi", gpuResource: "1"},
	}

	// Replay first, then the framework; both read the same loaded configuration,
	// so any drift between the offline table and the plugin shows up as a diff.
	want, err := replay.Table(loaded, workload, cluster)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	wantScores := make(map[string]int64, len(want.Rows))
	wantOrder := make([]string, 0, len(want.Rows))
	for _, row := range want.Rows {
		wantScores[row.Node] = row.Score
		wantOrder = append(wantOrder, row.Node)
	}

	e := newEnvWithConfig(t, cfgPath, loaded, nodes, resident, true)
	res := e.run(pod("gpu-inference-v1", workloadRequests))

	if got := statusCode(res.scoring); got != fwk.Success {
		t.Fatalf("scoring status = %v: %q", got, res.scoring.Message())
	}
	for _, nodeName := range []string{"gpu-a", "gpu-b", "gpu-c"} {
		if got := res.filterStatus[nodeName]; got != fwk.Success {
			t.Fatalf("%s filter verdict = %v, want success (message %q)", nodeName, got, res.filterMessage[nodeName])
		}
	}
	if got := res.filterStatus["cpu-d"]; !isUnschedulable(got) {
		t.Fatalf("cpu-d filter verdict = %v, want an unschedulable code: a GPU pod cannot fit a CPU-only node even though tahrs never penalises it in Score", got)
	}

	for _, nodeName := range []string{"gpu-a", "gpu-b", "gpu-c"} {
		gotScore, scored := res.scores[nodeName]
		if !scored {
			t.Errorf("node %s was not scored by the framework", nodeName)
			continue
		}
		if wantScore := wantScores[nodeName] * int64(scoreWeight); gotScore != wantScore {
			t.Errorf("node %s: framework score %d, replay score %d (weight %d)", nodeName, gotScore, wantScore/int64(scoreWeight), scoreWeight)
		}
	}

	// The CPU-only node is filtered by NodeResourcesFit before scoring; replay
	// still lists it, with the anomaly the evaluation reported and MinScore.
	if _, scored := res.scores["cpu-d"]; scored {
		t.Fatalf("cpu-d was scored by the framework: %v", res.scores)
	}
	var cpuRow *replay.Row
	for i := range want.Rows {
		if want.Rows[i].Node == "cpu-d" {
			cpuRow = &want.Rows[i]
		}
	}
	if cpuRow == nil {
		t.Fatalf("replay did not list cpu-d: %v", wantOrder)
	}
	if cpuRow.Score != scoring.MinScore {
		t.Errorf("replay cpu-d score = %d, want MinScore %d", cpuRow.Score, scoring.MinScore)
	}
	if cpuRow.Err == "" {
		t.Errorf("replay cpu-d row carries no anomaly: %+v", *cpuRow)
	}

	feasibleOrder := make([]string, 0, len(wantOrder))
	for _, nodeName := range wantOrder {
		if nodeName != "cpu-d" {
			feasibleOrder = append(feasibleOrder, nodeName)
		}
	}
	if ranking := res.ranking(); !equalStrings(ranking, feasibleOrder) {
		t.Fatalf("framework ranking %v, replay ranking %v", ranking, feasibleOrder)
	}
	if feasibleOrder[0] == "cpu-d" {
		t.Fatalf("replay ranked the CPU-only node first: %v", feasibleOrder)
	}
}

// TestAnomalousNodeGetsMinScore is T3 criterion 4: a node whose numbers cannot
// be evaluated must not abort the cycle, must score MinScore, and must not stop
// the pod from landing on a healthy node.
func TestAnomalousNodeGetsMinScore(t *testing.T) {
	nodes := []*v1.Node{
		// Allocatable resources exist, but no GPU capacity: Pack cannot evaluate
		// the GPU dimension and reports an anomaly.
		node("gpu-broken", "8000m", "32Gi", 0),
		node("gpu-ok", "8000m", "32Gi", 4),
	}
	requests := map[v1.ResourceName]string{
		v1.ResourceCPU:               "500m",
		v1.ResourceMemory:            "1Gi",
		v1.ResourceName(gpuResource): "1",
	}

	t.Run("score-fallback", func(t *testing.T) {
		// No native plugins: only the Tahrs plugin runs, so the malformed node
		// actually reaches Score.
		e := newEnv(t, gpuInferenceSetting(), nodes, false)
		res := e.run(pod("gpu-inference-v1", requests))

		if got := statusCode(res.scoring); got != fwk.Success {
			t.Fatalf("scoring status = %v, want success: the plugin must not fail the cycle (%q)", got, res.scoring.Message())
		}
		if got, ok := res.scores["gpu-broken"]; !ok || got != scoring.MinScore {
			t.Fatalf("gpu-broken score = %d (present=%t), want MinScore %d", got, ok, scoring.MinScore)
		}
		if got := res.scores["gpu-ok"]; got <= scoring.MinScore {
			t.Fatalf("gpu-ok score = %d, want > %d", got, scoring.MinScore)
		}
		ranking := res.ranking()
		if len(ranking) != 2 || ranking[0] != "gpu-ok" {
			t.Fatalf("ranking = %v, want [gpu-ok gpu-broken]", ranking)
		}
	})

	t.Run("native-fit-filters-it-first", func(t *testing.T) {
		// With NodeResourcesFit enabled the malformed node never reaches Score;
		// the plugin fallback is the last line of defence, not the only one.
		e := newEnv(t, gpuInferenceSetting(), nodes, true)
		res := e.run(pod("gpu-inference-v1", requests))

		if got := res.filterStatus["gpu-broken"]; !isUnschedulable(got) {
			t.Fatalf("gpu-broken filter verdict = %v, want an unschedulable code", got)
		}
		if res.selected != "gpu-ok" {
			t.Fatalf("selected %q, want gpu-ok: the only feasible node is returned without scoring", res.selected)
		}
	})
}

// TestSixteenNodesParallelScore is T3 criterion 5. Run with -race: the plugin is
// invoked concurrently for every node through the framework's parallelism.
func TestSixteenNodesParallelScore(t *testing.T) {
	nodes := make([]*v1.Node, 0, 16)
	for i := range 16 {
		// Vary the capacity so the nodes are not symmetric.
		nodes = append(nodes, node(
			fmt.Sprintf("node-%02d", i),
			fmt.Sprintf("%dm", 4000+(i%4)*2000),
			fmt.Sprintf("%dGi", 8+(i%3)*8),
			int64(i%3),
		))
	}
	e := newEnv(t, cpuGeneralSetting(), nodes, false)
	requests := map[v1.ResourceName]string{v1.ResourceCPU: "300m", v1.ResourceMemory: "512Mi"}

	first := e.run(pod("", requests))
	if got := statusCode(first.scoring); got != fwk.Success {
		t.Fatalf("scoring status = %v: %q", got, first.scoring.Message())
	}
	if len(first.scores) != 16 {
		t.Fatalf("scored %d nodes, want 16", len(first.scores))
	}

	// A second cycle must produce identical scores: no shared mutable state and
	// no dependence on scheduling order.
	for cycle := range 2 {
		next := e.run(pod("", requests))
		if len(next.scores) != 16 {
			t.Fatalf("cycle %d scored %d nodes, want 16", cycle+2, len(next.scores))
		}
		if !equalScores(first.scores, next.scores) {
			t.Fatalf("cycle %d scores differ from cycle 1: %v vs %v", cycle+2, next.scores, first.scores)
		}
	}
}

// TestScoreBoundsMirrorFramework pins the plugin's score range to the framework's.
func TestScoreBoundsMirrorFramework(t *testing.T) {
	if scoring.MinScore != int64(fwk.MinScore) {
		t.Errorf("scoring.MinScore = %d, framework MinScore = %d", scoring.MinScore, fwk.MinScore)
	}
	if scoring.MaxScore != int64(fwk.MaxScore) {
		t.Errorf("scoring.MaxScore = %d, framework MaxScore = %d", scoring.MaxScore, fwk.MaxScore)
	}
}

// ---------------------------------------------------------------------------
// small assertion helpers
// ---------------------------------------------------------------------------

// isUnschedulable reports whether a Filter verdict rejected the node. Upstream
// distinguishes "may fit later" from "will never fit"; both mean "not this node".
func isUnschedulable(code fwk.Code) bool {
	return code == fwk.Unschedulable || code == fwk.UnschedulableAndUnresolvable
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalScores(a, b map[string]int64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
