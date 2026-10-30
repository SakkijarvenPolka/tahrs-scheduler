package kubeadapter

// tahrs deliberately does not read ResourceSlices or DeviceClasses: the GPU
// dimension is fed by the extended resource the framework publishes, which the
// DRA bridge keeps in sync with the generated claims. These assertions pin the
// numbers that follow from that choice, so a later DRA integration cannot
// change them silently.

import (
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	fwkruntime "k8s.io/kubernetes/pkg/scheduler/framework"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	"k8s.io/utils/ptr"
)

// TestPodRequestsGPUExtendedResource pins the shape every GPU workload has
// before devices are allocated through DRA: the Pod asks for whole cards
// through the extended resource, which is the only GPU signal the ledger reads.
func TestPodRequestsGPUExtendedResource(t *testing.T) {
	pod := podWith([]v1.Container{container("trainer", "4", "16Gi", 1)}, nil)
	pod.Spec.Containers[0].Resources.Limits = v1.ResourceList{
		gpuResource: resource.MustParse("1"),
	}
	pod = defaultedPod(pod)

	if got := PodRequests(pod, gpuResource).GPUs; got != 1 {
		t.Fatalf("PodRequests().GPUs = %d, want 1", got)
	}
	// The framework agrees: one card, counted once, even though the Pod
	// declares both a request and a limit.
	if delta := ledgerGPUDelta(emptyNodeInfo(), pod); delta != 1 {
		t.Fatalf("framework ledger delta = %d, want 1", delta)
	}
}

// TestPodRequestsIgnoresDRAClaims pins the boundary of the adapter: a device
// held through a claim is not an extended resource request, so it must not be
// added to the vector on top of one.
func TestPodRequestsIgnoresDRAClaims(t *testing.T) {
	claimPod := podWith([]v1.Container{container("inference", "4", "16Gi", 0)}, nil)
	claimPod.Spec.ResourceClaims = []v1.PodResourceClaim{{
		Name:              "gpu",
		ResourceClaimName: ptr.To("inference-gpu"),
	}}
	claimPod.Spec.Containers[0].Resources.Claims = []v1.ResourceClaim{{Name: "gpu"}}

	if got := PodRequests(claimPod, gpuResource).GPUs; got != 0 {
		t.Fatalf("claim backed Pod GPUs = %d, want 0 (no extended resource request)", got)
	}

	// A bridged Pod keeps its manifest unchanged and records the generated claim
	// in the status; that status is not a request either.
	bridged := claimPod.DeepCopy()
	bridged.Status.ExtendedResourceClaimStatus = &v1.PodExtendedResourceClaimStatus{
		ResourceClaimName: "inference-gpu",
		RequestMappings: []v1.ContainerExtendedResourceRequest{{
			ContainerName: "inference",
			ResourceName:  string(gpuResource),
			RequestName:   "gpu",
		}},
	}
	if got := PodRequests(bridged, gpuResource).GPUs; got != 0 {
		t.Fatalf("bridged claim Pod GPUs = %d, want 0", got)
	}

	// When both signals exist the extended resource stays the single source of
	// truth, so the device is still counted once.
	both := bridged.DeepCopy()
	both.Spec.Containers[0].Resources.Requests[gpuResource] = *resource.NewQuantity(1, resource.DecimalSI)
	if got := PodRequests(both, gpuResource).GPUs; got != 1 {
		t.Fatalf("dual signal Pod GPUs = %d, want 1", got)
	}
}

// TestNodeSnapshotRemainingGPUs pins the arithmetic the packer relies on, and
// the blind spot it inherits: allocatable comes from the node object, so a node
// whose devices exist only as ResourceSlices advertises no GPU capacity even
// while Pods on it hold devices.
func TestNodeSnapshotRemainingGPUs(t *testing.T) {
	node := st.MakeNode().Name("gpu-1").Capacity(map[v1.ResourceName]string{
		v1.ResourceCPU:    "32",
		v1.ResourceMemory: "128Gi",
		gpuResource:       "4",
	}).Obj()
	first := podWith([]v1.Container{container("a", "1", "1Gi", 1)}, nil)
	second := podWith([]v1.Container{container("b", "1", "1Gi", 1)}, nil)

	nodeInfo := fwkruntime.NewNodeInfo(first, second)
	nodeInfo.SetNode(node)

	snapshot := NodeSnapshot(nodeInfo, gpuResource)
	if snapshot.Allocatable.GPUs != 4 || snapshot.Used.GPUs != 2 {
		t.Fatalf("snapshot = %+v, want allocatable 4 and used 2", snapshot)
	}
	if got := snapshot.Remaining().GPUs; got != 2 {
		t.Fatalf("Remaining().GPUs = %d, want 2", got)
	}

	// Blind spot: the node does not advertise the extended resource, so devices
	// held by its Pods are counted as usage but never as capacity.
	blindNode := fwkruntime.NewNodeInfo(first)
	blindNode.SetNode(st.MakeNode().Name("dra-only").Capacity(map[v1.ResourceName]string{
		v1.ResourceCPU:    "32",
		v1.ResourceMemory: "128Gi",
	}).Obj())

	blind := NodeSnapshot(blindNode, gpuResource)
	if blind.HasGPU() || blind.Allocatable.GPUs != 0 {
		t.Fatalf("DRA only node advertises GPUs: %+v", blind.Allocatable)
	}
	if blind.Used.GPUs != 1 || blind.Remaining().GPUs != 0 {
		t.Fatalf("DRA only snapshot = %+v, want used 1 and remaining 0", blind)
	}
}
