package kubeadapter

import (
	v1 "k8s.io/api/core/v1"
	resourcehelper "k8s.io/component-helpers/resource"

	"github.com/SakkijarvenPolka/tahrs-scheduler/internal/model"
)

// ResourcesFromList converts a Kubernetes ResourceList into the scoring model.
// Only the three dimensions the scheduler scores are extracted; every other
// resource (ephemeral storage, huge pages, custom extended resources) is
// ignored. GPU is read from gpuResourceName; an empty name disables the GPU
// dimension.
//
// Negative quantities cannot be produced by the API server but are clamped to
// zero here so that a hand-built object cannot corrupt the scoring arithmetic.
func ResourcesFromList(list v1.ResourceList, gpuResourceName v1.ResourceName) model.Resources {
	var out model.Resources
	if quantity, ok := list[v1.ResourceCPU]; ok {
		out.MilliCPU = quantity.MilliValue()
	}
	if quantity, ok := list[v1.ResourceMemory]; ok {
		out.MemoryBytes = quantity.Value()
	}
	if gpuResourceName != "" {
		if quantity, ok := list[gpuResourceName]; ok {
			out.GPUs = quantity.Value()
		}
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

// PodRequests returns the resource demand of a Pod across the scoring
// dimensions. The value is what the Pod is going to consume once it is bound,
// computed the same way the scheduler's NodeInfo ledger computes it, so that
// "requested + Pod demand <= allocatable" is a meaningful comparison.
//
// A Pod that was built in memory (tests, replay files, informer cache entries
// that never went through defaulting) may declare limits without requests.
// Kubernetes backfills missing requests from limits for every resource at the
// Pod level, so the adapter applies the same rule before aggregating. The copy
// is only made when a backfill is actually required, which keeps the hot path
// (Score runs once per candidate node) allocation free.
func PodRequests(pod *v1.Pod, gpuResourceName v1.ResourceName) model.TaskRequests {
	if pod == nil {
		return model.TaskRequests{}
	}
	effective := pod
	if needsLimitBackfill(pod) {
		effective = backfillRequestsFromLimits(pod)
	}
	requests := ResourcesFromList(
		resourcehelper.PodRequests(effective, resourcehelper.PodResourcesOptions{}),
		gpuResourceName,
	)
	if gpuResourceName == "" {
		return requests
	}
	// Extended resources are incompressible: a container that only declares a
	// limit still reserves the full amount for the whole lifetime of the Pod.
	// Taking the larger of requests and limits covers single containers whose
	// request is deliberately lower than the limit.
	if limits := ResourcesFromList(
		resourcehelper.PodLimits(effective, resourcehelper.PodResourcesOptions{}),
		gpuResourceName,
	); limits.GPUs > requests.GPUs {
		requests.GPUs = limits.GPUs
	}
	return requests
}

// needsLimitBackfill reports whether any container declares a limit for a
// resource for which it does not declare a request.
func needsLimitBackfill(pod *v1.Pod) bool {
	containers := make([]v1.Container, 0, len(pod.Spec.InitContainers)+len(pod.Spec.Containers))
	containers = append(containers, pod.Spec.InitContainers...)
	containers = append(containers, pod.Spec.Containers...)
	for _, container := range containers {
		for name := range container.Resources.Limits {
			if _, ok := container.Resources.Requests[name]; !ok {
				return true
			}
		}
	}
	return false
}

// backfillRequestsFromLimits returns a copy of the Pod whose containers declare
// a request for every resource they limit, matching the defaulting performed by
// the API server for v1.Pod objects.
func backfillRequestsFromLimits(pod *v1.Pod) *v1.Pod {
	copied := pod.DeepCopy()
	for i := range copied.Spec.InitContainers {
		fillRequestsFromLimits(&copied.Spec.InitContainers[i])
	}
	for i := range copied.Spec.Containers {
		fillRequestsFromLimits(&copied.Spec.Containers[i])
	}
	return copied
}

func fillRequestsFromLimits(container *v1.Container) {
	if len(container.Resources.Limits) == 0 {
		return
	}
	if container.Resources.Requests == nil {
		container.Resources.Requests = make(v1.ResourceList, len(container.Resources.Limits))
	}
	for name, quantity := range container.Resources.Limits {
		if _, ok := container.Resources.Requests[name]; !ok {
			container.Resources.Requests[name] = quantity.DeepCopy()
		}
	}
}
