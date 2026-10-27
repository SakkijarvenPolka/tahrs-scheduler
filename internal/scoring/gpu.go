package scoring

import "github.com/SakkijarvenPolka/tahrs-scheduler/internal/model"

// GPUPenalty returns the soft penalty applied to a GPU capable node for a Pod
// that does not request a GPU, plus whether it applies.
//
// It is a preference only. Hard isolation is expected from taints and node
// affinity (plan §5.3): a penalty can never stop a non-GPU Pod from being placed
// on a GPU node when that node is the only feasible one.
func GPUPenalty(q model.TaskRequests, snap model.NodeSnapshot, p model.ProfileView) (int64, bool) {
	if p.GPUPolicy != model.GPUPolicyAvoid {
		return 0, false
	}
	if q.GPUs != 0 {
		return 0, false
	}
	if !snap.HasGPU() {
		return 0, false
	}
	return p.GPUAvoidPenalty, true
}
