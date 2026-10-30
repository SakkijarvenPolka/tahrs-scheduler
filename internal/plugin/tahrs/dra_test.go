package tahrs

import (
	"context"
	"testing"

	v1 "k8s.io/api/core/v1"
	fwk "k8s.io/kube-scheduler/framework"
	schedframework "k8s.io/kubernetes/pkg/scheduler/framework"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	"k8s.io/utils/ptr"

	"github.com/SakkijarvenPolka/tahrs-scheduler/internal/kubeadapter"
	"github.com/SakkijarvenPolka/tahrs-scheduler/internal/model"
	"github.com/SakkijarvenPolka/tahrs-scheduler/internal/profile"
	"github.com/SakkijarvenPolka/tahrs-scheduler/internal/scoring"
)

// TestGPULedgerBlindReason covers the two shapes in which the GPU dimension
// cannot see a Pod's device demand, and the cycles it must stay quiet for.
func TestGPULedgerBlindReason(t *testing.T) {
	gpuNode := nodeInfo(st.MakeNode().Name("gpu").Capacity(map[v1.ResourceName]string{
		v1.ResourceCPU:    "32",
		v1.ResourceMemory: "128Gi",
		gpuResource:       "4",
	}).Obj())
	cpuNode := nodeInfo(st.MakeNode().Name("cpu").Capacity(map[v1.ResourceName]string{
		v1.ResourceCPU:    "32",
		v1.ResourceMemory: "128Gi",
	}).Obj())

	cases := []struct {
		name    string
		request model.TaskRequests
		claims  bool
		nodes   []fwk.NodeInfo
		want    string
	}{
		{
			name:    "node advertises the extended resource",
			request: model.Resources{GPUs: 1},
			nodes:   []fwk.NodeInfo{gpuNode},
		},
		{
			name:    "bridged request without allocatable source",
			request: model.Resources{GPUs: 1},
			nodes:   []fwk.NodeInfo{cpuNode},
			want:    ledgerBlindExtendedResource,
		},
		{
			name:   "claims without allocatable source",
			nodes:  []fwk.NodeInfo{cpuNode},
			claims: true,
			want:   ledgerBlindDeviceClaims,
		},
		{
			name:    "cpu pod on a cpu node",
			nodes:   []fwk.NodeInfo{cpuNode},
			request: model.Resources{MilliCPU: 500},
		},
		{
			name:    "no feasible nodes means Score never runs",
			request: model.Resources{GPUs: 1},
		},
		{
			name:    "nil node info is skipped",
			request: model.Resources{GPUs: 1},
			nodes:   []fwk.NodeInfo{nil, cpuNode},
			want:    ledgerBlindExtendedResource,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := gpuLedgerBlindReason(tc.request, tc.claims, tc.nodes, v1.ResourceName(gpuResource)); got != tc.want {
				t.Fatalf("gpuLedgerBlindReason() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPreScoreSurvivesBlindCycle: recording the degradation must not fail the
// Pod. PreScore is called with the feasible nodes, and a blind cycle still
// returns nil so Score can fall back per node (R7).
func TestPreScoreSurvivesBlindCycle(t *testing.T) {
	p := newTestPlugin(t, cpuSetting())
	state := schedframework.NewCycleState()
	state.Write(stateKey, &stateData{Requests: model.Resources{GPUs: 1}})
	pod := st.MakePod().Name("gpu-pod").Namespace("default").Obj()
	cpuNode := nodeInfo(st.MakeNode().Name("cpu").Capacity(map[v1.ResourceName]string{
		v1.ResourceCPU:    "8",
		v1.ResourceMemory: "16Gi",
	}).Obj())

	if status := p.PreScore(context.Background(), state, pod, []fwk.NodeInfo{cpuNode}); status != nil {
		t.Fatalf("PreScore status = %v, want nil", status)
	}
}

// TestScoreSkipsGPUPenaltyForDeviceClaims pins the wiring: the option is
// derived from the Pod inside Score, so a claim backed Pod lands on a GPU node
// without the avoid penalty even though its request vector has no GPU.
func TestScoreSkipsGPUPenaltyForDeviceClaims(t *testing.T) {
	p := newTestPlugin(t, cpuSetting())
	state := schedframework.NewCycleState()

	n := st.MakeNode().Name("gpu-a").Capacity(map[v1.ResourceName]string{
		v1.ResourceCPU:    "8000m",
		v1.ResourceMemory: "32Gi",
		gpuResource:       "4",
	}).Obj()
	pod := st.MakePod().Name("claim-workload").Namespace("default").Req(map[v1.ResourceName]string{
		v1.ResourceCPU:    "4",
		v1.ResourceMemory: "8Gi",
	}).Obj()
	pod.Spec.ResourceClaims = []v1.PodResourceClaim{{
		Name:              "gpu",
		ResourceClaimName: ptr.To("claim-1"),
	}}

	resolution, err := profile.Resolve(nil, p.setting.DefaultProfile, p.views)
	if err != nil {
		t.Fatalf("resolve profile: %v", err)
	}
	requests := kubeadapter.PodRequests(pod, v1.ResourceName(p.setting.GPUResourceName))
	state.Write(stateKey, &stateData{Resolution: resolution, Requests: requests})

	score, status := p.Score(context.Background(), state, pod, nodeInfo(n))
	if status != nil {
		t.Fatalf("Score status = %v, want nil", status)
	}

	snapshot := model.NodeSnapshot{
		Name:        "gpu-a",
		Allocatable: model.Resources{MilliCPU: 8000, MemoryBytes: 32 << 30, GPUs: 4},
	}
	want, err := scoring.Evaluate(requests, snapshot, resolution.Profile, scoring.WithDeviceClaims(true))
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if score != want.Final {
		t.Fatalf("Score = %d, direct evaluation with claims = %d", score, want.Final)
	}

	penalised, err := scoring.Evaluate(requests, snapshot, resolution.Profile)
	if err != nil {
		t.Fatalf("evaluate without claims: %v", err)
	}
	if penalised.GPUPenalty == 0 {
		t.Fatal("expected the avoid policy to apply without claims")
	}
	if score <= penalised.Final {
		t.Fatalf("claims did not lift the score: %d <= %d", score, penalised.Final)
	}
}
