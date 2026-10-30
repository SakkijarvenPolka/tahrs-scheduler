package scoring

import "testing"

// TestDeviceClaimsSkipGPUPenalty pins scoring.WithDeviceClaims: a Pod that can
// hold a device through a claim carries no GPU count in its request vector, so
// the avoid policy would otherwise push it off exactly the nodes it targets.
func TestDeviceClaimsSkipGPUPenalty(t *testing.T) {
	// Half of the node is free in both dimensions, so the combined score is
	// 0.3*Fit + 0.7*Pack = 0.55 and the penalty stays visible in Final instead
	// of being floored away at MinScore.
	gpuNode := node("gpu-1", res(8000, 16, 4), res(0, 0, 0))
	request := res(4000, 8, 0)

	plain, err := Evaluate(request, gpuNode, profileCPUGeneral)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if plain.GPUPenalty != 30 {
		t.Fatalf("plain GPUPenalty = %d, want 30", plain.GPUPenalty)
	}
	if plain.Final != plain.PrePenalty-30 {
		t.Fatalf("plain Final = %d, want PrePenalty-30 = %d", plain.Final, plain.PrePenalty-30)
	}

	claim, err := Evaluate(request, gpuNode, profileCPUGeneral, WithDeviceClaims(true))
	if err != nil {
		t.Fatalf("Evaluate with claims: %v", err)
	}
	if claim.GPUPenalty != 0 {
		t.Fatalf("claim backed GPUPenalty = %d, want 0", claim.GPUPenalty)
	}
	if claim.Final != claim.PrePenalty {
		t.Fatalf("claim backed Final = %d, want PrePenalty %d", claim.Final, claim.PrePenalty)
	}
	if claim.Final <= plain.Final {
		t.Fatalf("claims made the node less attractive: %d <= %d", claim.Final, plain.Final)
	}

	// An explicit false keeps the plain behaviour, so a caller can pass the
	// condition straight through without branching.
	explicit, err := Evaluate(request, gpuNode, profileCPUGeneral, WithDeviceClaims(false))
	if err != nil {
		t.Fatalf("Evaluate with explicit false: %v", err)
	}
	if explicit != plain {
		t.Fatalf("WithDeviceClaims(false) = %+v, want %+v", explicit, plain)
	}

	// The option never reactivates the penalty for a Pod that does ask for a
	// GPU through the extended resource.
	if penalty, applied := GPUPenalty(res(4000, 8, 1), gpuNode, profileCPUGeneral, WithDeviceClaims(true)); applied || penalty != 0 {
		t.Fatalf("GPUPenalty for a GPU requesting Pod = %d/%v, want 0/false", penalty, applied)
	}
}

// TestGPURequestWithoutClaimsIsPenalised guards the other direction: dropping
// the penalty must be driven by the Pod's claims, not by the absence of a GPU
// request alone.
func TestGPURequestWithoutClaimsIsPenalised(t *testing.T) {
	gpuNode := node("gpu-1", res(8000, 16, 4), res(0, 0, 0))

	bd, err := Evaluate(res(4000, 8, 0), gpuNode, profileCPUGeneral)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if bd.GPUPenalty != profileCPUGeneral.GPUAvoidPenalty {
		t.Fatalf("GPUPenalty = %d, want %d", bd.GPUPenalty, profileCPUGeneral.GPUAvoidPenalty)
	}

	// A GPU node without the resource in allocatable cannot be penalised: it is
	// not a GPU node from the model's point of view.
	cpuNode := node("cpu-1", res(8000, 16, 0), res(0, 0, 0))
	if bd, err := Evaluate(res(4000, 8, 0), cpuNode, profileCPUGeneral); err != nil || bd.GPUPenalty != 0 {
		t.Fatalf("CPU node GPUPenalty = %d (err %v), want 0", bd.GPUPenalty, err)
	}
}
