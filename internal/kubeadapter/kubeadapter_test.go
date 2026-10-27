package kubeadapter

import (
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	fwkruntime "k8s.io/kubernetes/pkg/scheduler/framework"
	st "k8s.io/kubernetes/pkg/scheduler/testing"

	"github.com/SakkijarvenPolka/tahrs-scheduler/internal/model"
)

const gpuResource = v1.ResourceName("nvidia.com/gpu")

func TestPodRequestsMatchesNodeInfoLedger(t *testing.T) {
	overhead := v1.ResourceList{
		v1.ResourceCPU:    resource.MustParse("250m"),
		v1.ResourceMemory: resource.MustParse("200Mi"),
	}

	cases := []struct {
		name string
		pod  *v1.Pod
		want model.Resources
	}{
		{
			// Init containers define a resource floor: the Pod never needs more
			// than the largest init container plus the sum of restartable ones.
			name: "init container sets the floor",
			pod: podWith(
				[]v1.Container{
					container("app", "500m", "512Mi", 0),
				},
				[]v1.Container{
					container("migrate", "2", "2Gi", 0),
				},
			),
			want: model.Resources{MilliCPU: 2000, MemoryBytes: 2 * gib},
		},
		{
			// Native sidecars (restartable init containers) accumulate for the
			// whole Pod lifetime and are added to every later init container.
			name: "restartable init container accumulates",
			pod: podWith(
				[]v1.Container{
					container("app", "500m", "512Mi", 0),
				},
				[]v1.Container{
					sidecar("istio-proxy", "300m", "256Mi", 0),
					container("migrate", "1", "1Gi", 0),
				},
			),
			want: model.Resources{MilliCPU: 1300, MemoryBytes: 1280 * mib},
		},
		{
			// Pod overhead (runtime class) is added on top of container requests.
			name: "pod overhead is added",
			pod: func() *v1.Pod {
				pod := podWith([]v1.Container{container("app", "500m", "512Mi", 0)}, nil)
				pod.Spec.Overhead = overhead
				return pod
			}(),
			want: model.Resources{MilliCPU: 750, MemoryBytes: 712 * mib},
		},
		{
			// Extended resources are routinely declared through limits only.
			name: "gpu declared only in limits",
			pod:  podWith([]v1.Container{gpuLimitsOnly("app", "500m", "512Mi", 2)}, nil),
			want: model.Resources{MilliCPU: 500, MemoryBytes: 512 * mib, GPUs: 2},
		},
		{
			// The normal case: extended resources are always requested and
			// limited to the same amount, which the ledger counts once.
			name: "gpu declared as request and limit",
			pod: func() *v1.Pod {
				pod := podWith([]v1.Container{container("app", "4", "8Gi", 2)}, nil)
				pod.Spec.Containers[0].Resources.Limits = v1.ResourceList{
					gpuResource: resource.MustParse("2"),
				}
				return pod
			}(),
			want: model.Resources{MilliCPU: 4000, MemoryBytes: 8 * gib, GPUs: 2},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The API server backfills missing requests from limits; mirror that
			// so the framework ledger sees the same object a real cluster would.
			pod := defaultedPod(tc.pod)

			got := PodRequests(pod, gpuResource)
			if got != tc.want {
				t.Fatalf("PodRequests() = %+v, want %+v", got, tc.want)
			}

			// Cross-check against the scheduler's own ledger: adding the Pod to a
			// NodeInfo must raise its requested resources by exactly the same
			// amount in every dimension.
			// GetRequested returns a pointer to a live resource that AddPod
			// mutates in place, so snapshot the numbers before adding the Pod.
			nodeInfo := emptyNodeInfo()
			before := ledgerSnapshot(nodeInfo)
			nodeInfo.AddPod(pod)
			after := ledgerSnapshot(nodeInfo)

			delta := model.Resources{
				MilliCPU:    after.MilliCPU - before.MilliCPU,
				MemoryBytes: after.MemoryBytes - before.MemoryBytes,
				GPUs:        after.GPUs - before.GPUs,
			}
			if delta != got {
				t.Fatalf("NodeInfo ledger delta = %+v, adapter computed %+v", delta, got)
			}
		})
	}
}

// TestGPULimitsOnlyWithoutDefaulting documents why the adapter backfills:
// the framework ledger only counts what is in the requests maps, so a Pod that
// bypassed API server defaulting (in-memory object, replay file) would lose its
// GPU demand and be scored as a CPU-only Pod.
func TestGPULimitsOnlyWithoutDefaulting(t *testing.T) {
	raw := podWith([]v1.Container{gpuLimitsOnly("app", "500m", "512Mi", 2)}, nil)

	if delta := ledgerGPUDelta(emptyNodeInfo(), raw); delta != 0 {
		t.Fatalf("unexpected framework behaviour: raw limits-only Pod counted %d GPUs", delta)
	}
	if got := PodRequests(raw, gpuResource); got.GPUs != 2 {
		t.Fatalf("PodRequests().GPUs = %d, want 2 (backfilled from limits)", got.GPUs)
	}
}

// TestGPURequestBelowLimitIsReservedAtLimit pins the one place where the
// adapter is intentionally more conservative than the framework ledger: a
// container that requests one GPU but may use two keeps a GPU device attached
// for its whole lifetime, so the packer must assume two.
func TestGPURequestBelowLimitIsReservedAtLimit(t *testing.T) {
	pod := podWith([]v1.Container{container("app", "4", "8Gi", 1)}, nil)
	pod.Spec.Containers[0].Resources.Limits = v1.ResourceList{
		gpuResource: resource.MustParse("2"),
	}

	nodeInfo := emptyNodeInfo()
	before := ledgerSnapshot(nodeInfo)
	nodeInfo.AddPod(pod)
	if delta := ledgerSnapshot(nodeInfo).GPUs - before.GPUs; delta != 1 {
		t.Fatalf("framework ledger counted %d GPUs, want 1", delta)
	}
	if got := PodRequests(pod, gpuResource).GPUs; got != 2 {
		t.Fatalf("PodRequests().GPUs = %d, want 2", got)
	}
}

func TestNodeSnapshot(t *testing.T) {
	node := st.MakeNode().Name("gpu-1").Capacity(map[v1.ResourceName]string{
		v1.ResourceCPU:    "4",
		v1.ResourceMemory: "8Gi",
		gpuResource:       "2",
	}).Obj()
	running := podWith([]v1.Container{container("worker", "1", "1Gi", 1)}, nil)

	nodeInfo := fwkruntime.NewNodeInfo(running)
	nodeInfo.SetNode(node)

	snapshot := NodeSnapshot(nodeInfo, gpuResource)
	if snapshot.Name != "gpu-1" {
		t.Errorf("name = %q, want gpu-1", snapshot.Name)
	}
	wantAllocatable := model.Resources{MilliCPU: 4000, MemoryBytes: 8 * gib, GPUs: 2}
	wantUsed := model.Resources{MilliCPU: 1000, MemoryBytes: 1 * gib, GPUs: 1}
	if snapshot.Allocatable != wantAllocatable {
		t.Errorf("allocatable = %+v, want %+v", snapshot.Allocatable, wantAllocatable)
	}
	if snapshot.Used != wantUsed {
		t.Errorf("used = %+v, want %+v", snapshot.Used, wantUsed)
	}
	wantRemaining := model.Resources{MilliCPU: 3000, MemoryBytes: 7 * gib, GPUs: 1}
	if got := snapshot.Remaining(); got != wantRemaining {
		t.Errorf("remaining = %+v, want %+v", got, wantRemaining)
	}
	if !snapshot.HasGPU() {
		t.Error("HasGPU() = false, want true")
	}

	// A node without the extended resource advertises zero GPUs.
	cpuOnly := fwkruntime.NewNodeInfo()
	cpuOnly.SetNode(st.MakeNode().Name("cpu-1").Capacity(map[v1.ResourceName]string{
		v1.ResourceCPU:    "8",
		v1.ResourceMemory: "16Gi",
	}).Obj())
	plain := NodeSnapshot(cpuOnly, gpuResource)
	if plain.Allocatable.GPUs != 0 || plain.HasGPU() {
		t.Errorf("CPU-only node reports GPUs: %+v", plain.Allocatable)
	}

	// Disabling the GPU dimension hides scalar resources entirely.
	withoutGPU := NodeSnapshot(nodeInfo, "")
	if withoutGPU.Allocatable.GPUs != 0 || withoutGPU.Used.GPUs != 0 {
		t.Errorf("gpu dimension not disabled: %+v", withoutGPU)
	}

	// A nil NodeInfo must not panic.
	if got := NodeSnapshot(nil, gpuResource); got != (model.NodeSnapshot{}) {
		t.Errorf("NodeSnapshot(nil) = %+v, want zero value", got)
	}
}

func TestResourcesFromListClampsNegatives(t *testing.T) {
	list := v1.ResourceList{
		v1.ResourceCPU:    resource.MustParse("-1"),
		v1.ResourceMemory: resource.MustParse("-1Gi"),
		gpuResource:       resource.MustParse("-2"),
	}
	if got := ResourcesFromList(list, gpuResource); got != (model.Resources{}) {
		t.Fatalf("ResourcesFromList() = %+v, want zero value", got)
	}
}

func TestPodRequestsNilPod(t *testing.T) {
	if got := PodRequests(nil, gpuResource); got != (model.TaskRequests{}) {
		t.Fatalf("PodRequests(nil) = %+v, want zero value", got)
	}
}

const (
	mib = int64(1) << 20
	gib = int64(1) << 30
)

func podWith(containers, initContainers []v1.Container) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "default"},
		Spec: v1.PodSpec{
			Containers:     containers,
			InitContainers: initContainers,
		},
	}
}

func container(name, cpu, memory string, gpus int64) v1.Container {
	c := v1.Container{
		Name: name,
		Resources: v1.ResourceRequirements{
			Requests: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse(cpu),
				v1.ResourceMemory: resource.MustParse(memory),
			},
		},
	}
	if gpus > 0 {
		c.Resources.Requests[gpuResource] = *resource.NewQuantity(gpus, resource.DecimalSI)
	}
	return c
}

// gpuLimitsOnly declares the GPU through limits only, the shape most GPU
// workloads use in practice.
func gpuLimitsOnly(name, cpu, memory string, gpus int64) v1.Container {
	c := container(name, cpu, memory, 0)
	c.Resources.Limits = v1.ResourceList{
		gpuResource: *resource.NewQuantity(gpus, resource.DecimalSI),
	}
	return c
}

// sidecar builds a restartable init container (native sidecar).
func sidecar(name, cpu, memory string, gpus int64) v1.Container {
	c := container(name, cpu, memory, gpus)
	always := v1.ContainerRestartPolicyAlways
	c.RestartPolicy = &always
	return c
}

func emptyNodeInfo() *fwkruntime.NodeInfo {
	nodeInfo := fwkruntime.NewNodeInfo()
	nodeInfo.SetNode(st.MakeNode().Name("n1").Capacity(map[v1.ResourceName]string{
		v1.ResourceCPU:    "32",
		v1.ResourceMemory: "128Gi",
		gpuResource:       "8",
	}).Obj())
	return nodeInfo
}

func ledgerSnapshot(nodeInfo *fwkruntime.NodeInfo) model.Resources {
	requested := nodeInfo.GetRequested()
	return model.Resources{
		MilliCPU:    requested.GetMilliCPU(),
		MemoryBytes: requested.GetMemory(),
		GPUs:        requested.GetScalarResources()[gpuResource],
	}
}

func ledgerGPUDelta(nodeInfo *fwkruntime.NodeInfo, pod *v1.Pod) int64 {
	before := ledgerSnapshot(nodeInfo).GPUs
	nodeInfo.AddPod(pod)
	return ledgerSnapshot(nodeInfo).GPUs - before
}

// defaultedPod applies the API server's request-from-limit defaulting to an
// in-memory Pod so that tests exercise the shape the scheduler actually sees.
func defaultedPod(pod *v1.Pod) *v1.Pod {
	return backfillRequestsFromLimits(pod)
}
