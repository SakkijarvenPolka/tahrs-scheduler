// Package scoring implements the tahrs scoring functions.
//
// It is a pure function library: no k8s.io imports, no I/O, no global mutable
// state. Everything here is deterministic and unit testable (plan §3.2).
package scoring

import (
	"math"

	"github.com/SakkijarvenPolka/tahrs-scheduler/internal/model"
)

// Neutral is the value used when a scoring component has no dimension left to
// evaluate (for example, a Pod with an empty request vector has nothing to
// match structurally). 0.5 means "no opinion": it neither rewards nor punishes
// the node, and it keeps the component comparable with a real [0,1] value.
const Neutral = 0.5

// Fit returns the weighted Bray-Curtis structural match between the Pod request
// and the node's free capacity:
//
//	t_k = q_k / s_k,  f_k = F_k / s_k
//	S   = 1 - Σ w_k|f_k - t_k| / Σ w_k(f_k + t_k)
//
// Dimensions with a zero request or a zero weight are skipped. When nothing is
// left to evaluate, Neutral is returned and used=false.
//
// Honest note: the maximum is reached when the free capacity equals the
// request, so the component is best-fit oriented and its ordering is largely
// aligned with Pack (F12). It replaced a cosine similarity because it reacts to
// the magnitude of the residual capacity, not because it provides an
// orthogonal signal.
func Fit(q model.TaskRequests, snap model.NodeSnapshot, w model.Weights) (value float64, used bool) {
	remaining := snap.Remaining()

	var numerator, denominator float64
	evaluated := 0
	for _, dim := range model.Dimensions {
		qk := q.Get(dim)
		wk := w.Get(dim)
		if qk == 0 || wk == 0 {
			continue
		}
		scale := float64(model.Scales.Get(dim))
		tk := float64(qk) / scale
		fk := float64(remaining.Get(dim)) / scale
		numerator += wk * math.Abs(fk-tk)
		denominator += wk * (fk + tk)
		evaluated++
	}
	if evaluated == 0 || denominator == 0 {
		return Neutral, false
	}
	return 1 - numerator/denominator, true
}
