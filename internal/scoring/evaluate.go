package scoring

import (
	"fmt"
	"math"

	"github.com/SakkijarvenPolka/tahrs-scheduler/internal/model"
)

// MaxScore mirrors fwk.MaxScore. It is duplicated here so the library does not
// depend on the scheduler API, and asserted against the real constant in
// test/framework.
const MaxScore = 100

// MinScore mirrors fwk.MinScore.
const MinScore = 0

// Evaluate combines the two scoring components and the GPU penalty into the
// integer score a Score plugin returns:
//
//	combined = clamp(alpha*Fit + beta*Pack, 0, 1)
//	Final    = max(0, round(100*combined) - penalty)
//
// The order matters and is covered by tests: clamping happens before rounding,
// the GPU penalty is subtracted after rounding, and the result is then floored
// at MinScore. Rounding after the penalty would make the penalty quantisation
// dependent and could produce a negative score.
//
// Options carry Pod properties that the resource vector cannot express. They do
// not change the arithmetic, only which terms apply (see WithDeviceClaims).
func Evaluate(q model.TaskRequests, snap model.NodeSnapshot, p model.ProfileView, opts ...Option) (model.Breakdown, error) {
	if err := p.Validate(); err != nil {
		return model.Breakdown{}, err
	}

	fit, fitUsed := Fit(q, snap, p.FitWeights)
	if !fitUsed {
		fit = Neutral
	}
	pack, packUsed, err := Pack(q, snap, p.PackWeights)
	if err != nil {
		return model.Breakdown{}, err
	}
	if !packUsed {
		pack = Neutral
	}

	combined := clamp01(p.Alpha*fit + p.Beta*pack)

	// Fit, Pack and the combined value are all checked: a NaN coefficient makes
	// every comparison false, so it would otherwise survive the clamp and turn
	// into MinInt64 when converted to an integer.
	for _, v := range []struct {
		name  string
		value float64
	}{{"fit", fit}, {"pack", pack}, {"combined", combined}} {
		if math.IsNaN(v.value) || math.IsInf(v.value, 0) {
			return model.Breakdown{}, fmt.Errorf("%w: %s=%v (profile %q, node %q)",
				model.ErrInvalidNumeric, v.name, v.value, p.Name, snap.Name)
		}
	}

	prePenalty := int64(math.Round(MaxScore * combined))
	penalty, applied := GPUPenalty(q, snap, p, opts...)
	if !applied {
		penalty = 0
	}
	final := scoreFromComponents(combined, penalty)

	return model.Breakdown{
		Profile:     p.Name,
		Fit:         fit,
		FitSkipped:  !fitUsed,
		Pack:        pack,
		PackSkipped: !packUsed,
		Combined:    combined,
		PrePenalty:  prePenalty,
		GPUPenalty:  penalty,
		Final:       final,
	}, nil
}

// scoreFromComponents applies the fixed composition order
// clamp -> round -> subtract penalty -> floor at MinScore.
//
// It is split out so the order can be pinned by tests with exact inputs
// (0.999 with penalty 30 must yield 70, 0.25 with penalty 30 must yield 0).
func scoreFromComponents(combined float64, penalty int64) int64 {
	score := int64(math.Round(MaxScore*clamp01(combined))) - penalty
	if score < MinScore {
		return MinScore
	}
	if score > MaxScore {
		return MaxScore
	}
	return score
}

// clamp01 confines v to [0,1]. NaN is not handled here on purpose: it is
// detected by the caller so the anomaly is counted instead of silently folded
// into the clamp.
func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
