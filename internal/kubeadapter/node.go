package kubeadapter

import (
	v1 "k8s.io/api/core/v1"
	fwk "k8s.io/kube-scheduler/framework"

	"github.com/SakkijarvenPolka/tahrs-scheduler/internal/model"
)

// NodeSnapshot builds the scoring view of a node from the scheduler framework's
// NodeInfo. Allocatable and usage are taken from the framework's own ledger, so
// the plugin scores exactly the state the rest of the scheduling cycle sees.
//
// The node is identified by its name; a NodeInfo without a Node object yields an
// empty name, which is fine for tests but never happens in a real cycle.
func NodeSnapshot(nodeInfo fwk.NodeInfo, gpuResourceName v1.ResourceName) model.NodeSnapshot {
	if nodeInfo == nil {
		return model.NodeSnapshot{}
	}
	name := ""
	if node := nodeInfo.Node(); node != nil {
		name = node.Name
	}
	return SnapshotFromResources(name, nodeInfo.GetAllocatable(), nodeInfo.GetRequested(), gpuResourceName)
}

// SnapshotFromResources assembles a snapshot from the two framework resource
// views. It exists separately from NodeSnapshot so that callers replaying a
// recorded cluster state can build a snapshot without a NodeInfo implementation.
func SnapshotFromResources(name string, allocatable, requested fwk.Resource, gpuResourceName v1.ResourceName) model.NodeSnapshot {
	return model.NodeSnapshot{
		Name:        name,
		Allocatable: resourceView(allocatable, gpuResourceName),
		Used:        resourceView(requested, gpuResourceName),
	}
}

func resourceView(resource fwk.Resource, gpuResourceName v1.ResourceName) model.Resources {
	if resource == nil {
		return model.Resources{}
	}
	var out model.Resources
	out.MilliCPU = resource.GetMilliCPU()
	out.MemoryBytes = resource.GetMemory()
	if gpuResourceName != "" {
		out.GPUs = resource.GetScalarResources()[gpuResourceName]
	}
	if out.MilliCPU < 0 {
		out.MilliCPU = 0
	}
	if out.MemoryBytes < 0 {
		out.MemoryBytes = 0
	}
	if out.GPUs < 0 {
		out.GPUs = 0
	}
	return out
}
