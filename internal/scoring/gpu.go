package scoring

import "github.com/SakkijarvenPolka/tahrs-scheduler/internal/model"

// GPUPenalty returns the soft penalty applied to a GPU capable node for a Pod
// that does not request a GPU, plus whether it applies.
//
// It is a preference only. Hard isolation is expected from taints and node
// affinity (plan §5.3): a penalty can never stop a non-GPU Pod from being placed
// on a GPU node when that node is the only feasible one.
func GPUPenalty(q model.TaskRequests, snap model.NodeSnapshot, p model.ProfileView, opts ...Option) (int64, bool) {
	if resolveOptions(opts).deviceClaims {
		// The Pod may hold a device through a claim while its request vector
		// carries no GPU count. Penalising it would push claim-backed workloads
		// off the GPU nodes they were written for, and the policy exists to keep
		// CPU workloads away from scarce cards, not to guess at device demand
		// the ledger cannot see.
		return 0, false
	}
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
