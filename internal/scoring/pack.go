package scoring

import (
	"fmt"
	"math"

	"github.com/SakkijarvenPolka/tahrs-scheduler/internal/model"
)

// Pack returns the packing score of the node after the Pod is placed:
//
//	u'_k = (U_k + q_k) / A_k
//	S    = 1 - sqrt( Σ v_k(1 - u'_k)^2 / Σ v_k )
//
// A dimension is skipped when it has no weight, or when the node has no
// allocatable capacity for it and the Pod does not request it. A node with zero
// allocatable capacity for a requested dimension is an anomaly (a Filter plugin
// should have rejected it) and is reported as model.ErrGPUNodeInsufficient so
// the Score extension point can fall back to MinScore instead of failing the
// scheduling cycle (R7).
func Pack(q model.TaskRequests, snap model.NodeSnapshot, v model.Weights) (value float64, used bool, err error) {
	var accumulator, weightSum float64
	evaluated := 0
	for _, dim := range model.Dimensions {
		qk := q.Get(dim)
		vk := v.Get(dim)
		ak := snap.Allocatable.Get(dim)
		if ak == 0 {
			if qk == 0 {
				continue
			}
			return 0, false, fmt.Errorf("%w: node %q dimension %q requested=%d allocatable=0",
				model.ErrGPUNodeInsufficient, snap.Name, dim, qk)
		}
		if vk == 0 {
			continue
		}
		usedAfter := float64(snap.Used.Get(dim)+qk) / float64(ak)
		d := 1 - usedAfter
		accumulator += vk * d * d
		weightSum += vk
		evaluated++
	}
	if evaluated == 0 || weightSum == 0 {
		return Neutral, false, nil
	}
	value = 1 - math.Sqrt(accumulator/weightSum)
	// A node can be over-committed (used+requested > allocatable). u' then
	// exceeds 1 and the raw value goes negative; such a node is the worst
	// possible packing choice, so the component is floored at 0.
	if value < 0 {
		value = 0
	}
	return value, true, nil
}
