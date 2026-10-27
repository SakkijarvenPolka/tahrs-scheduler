// Package model holds the pure data types shared by the tahrs scoring pipeline.
//
// Hard rule (see the project plan §3.2): this package must never import
// k8s.io/*. It is the boundary that lets the scoring library be unit tested
// without a cluster and reused by the offline replay tool.
package model

// Dimension is one axis of the resource vector handled by the scheduler.
type Dimension string

const (
	DimCPU    Dimension = "cpu"
	DimMemory Dimension = "memory"
	DimGPU    Dimension = "gpu"
)

// Dimensions is the fixed evaluation order every scoring function iterates over.
//
// Scores must be reproducible bit-for-bit for the same input (plan T1), so the
// scoring code ranges over this slice and never over a map: Go randomises map
// iteration order, which would make float accumulation order dependent.
var Dimensions = [...]Dimension{DimCPU, DimMemory, DimGPU}

// Resources is a three-dimensional resource vector: milli-CPU, memory in bytes,
// and a count of whole GPU cards of one configurable extended resource.
type Resources struct {
	MilliCPU    int64
	MemoryBytes int64
	GPUs        int64
}

// Scales is the fixed normalisation scale of each dimension. There is
// deliberately no user configurable scale (plan §4.1): a configurable scale
// makes profile parameters impossible to reason about across clusters.
var Scales = Resources{MilliCPU: 1000, MemoryBytes: 1 << 30, GPUs: 1}

// Get returns the value of one dimension.
func (r Resources) Get(d Dimension) int64 {
	switch d {
	case DimCPU:
		return r.MilliCPU
	case DimMemory:
		return r.MemoryBytes
	case DimGPU:
		return r.GPUs
	default:
		return 0
	}
}

// With returns a copy of r with one dimension replaced.
func (r Resources) With(d Dimension, v int64) Resources {
	switch d {
	case DimCPU:
		r.MilliCPU = v
	case DimMemory:
		r.MemoryBytes = v
	case DimGPU:
		r.GPUs = v
	}
	return r
}

// Add returns r + o dimension-wise.
func (r Resources) Add(o Resources) Resources {
	return Resources{
		MilliCPU:    r.MilliCPU + o.MilliCPU,
		MemoryBytes: r.MemoryBytes + o.MemoryBytes,
		GPUs:        r.GPUs + o.GPUs,
	}
}

// Sub returns r - o dimension-wise. The result may be negative; callers that
// need remaining capacity must clamp with Remaining.
func (r Resources) Sub(o Resources) Resources {
	return Resources{
		MilliCPU:    r.MilliCPU - o.MilliCPU,
		MemoryBytes: r.MemoryBytes - o.MemoryBytes,
		GPUs:        r.GPUs - o.GPUs,
	}
}

// TaskRequests is the request vector of the Pod being scheduled.
//
// It is an alias, not a distinct type: the scoring formulas are symmetric in
// requests and capacities and treating them as one type keeps the arithmetic
// helpers (Add/Sub/Get) usable on both sides.
type TaskRequests = Resources

// NodeSnapshot is the node state the scoring library needs: allocatable
// capacity and the resources already requested by pods bound to the node
// (assumed pods included). It intentionally carries no k8s objects.
type NodeSnapshot struct {
	Name        string
	Allocatable Resources
	// Used is the sum of GetRequested() over all pods on the node. It is the
	// native scheduler ledger, never GetNonZeroRequested() (plan red line).
	Used Resources
}

// Remaining returns the free capacity, clamped at zero per dimension.
//
// A node can be over-committed (Remaining would go negative) when pods were
// bound by another scheduler or when requests were edited in place. Clamping
// keeps the Bray-Curtis ratio inside [0, 1] instead of letting an over-committed
// node produce a nonsensical negative "free fraction".
func (n NodeSnapshot) Remaining() Resources {
	r := n.Allocatable.Sub(n.Used)
	return Resources{
		MilliCPU:    clampNonNegative(r.MilliCPU),
		MemoryBytes: clampNonNegative(r.MemoryBytes),
		GPUs:        clampNonNegative(r.GPUs),
	}
}

// HasGPU reports whether the node advertises at least one GPU card.
func (n NodeSnapshot) HasGPU() bool { return n.Allocatable.GPUs > 0 }

func clampNonNegative(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}
