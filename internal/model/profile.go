package model

import (
	"errors"
	"fmt"
)

// GPUPolicy controls the soft preference applied to GPU capable nodes when the
// Pod does not request a GPU.
type GPUPolicy string

const (
	// GPUPolicyAllow leaves GPU nodes unscored-penalised.
	GPUPolicyAllow GPUPolicy = "allow"
	// GPUPolicyAvoid subtracts the penalty from GPU nodes for non-GPU pods.
	GPUPolicyAvoid GPUPolicy = "avoid"
)

// Weights holds the per-dimension weight of one scoring component.
type Weights struct {
	CPU    float64
	Memory float64
	GPU    float64
}

// Get returns the weight of one dimension.
func (w Weights) Get(d Dimension) float64 {
	switch d {
	case DimCPU:
		return w.CPU
	case DimMemory:
		return w.Memory
	case DimGPU:
		return w.GPU
	default:
		return 0
	}
}

// IsZero reports whether every dimension has a zero weight.
func (w Weights) IsZero() bool {
	return w.CPU == 0 && w.Memory == 0 && w.GPU == 0
}

// Validate rejects negative weights.
func (w Weights) Validate(name string) error {
	for _, d := range Dimensions {
		if v := w.Get(d); v < 0 {
			return fmt.Errorf("%s weight for %q is negative: %v", name, d, v)
		}
	}
	return nil
}

// ProfileView is the resolved, scoring-ready view of a named profile. The
// plugin resolves it once per scheduling cycle (PreFilter) so that Score only
// reads immutable data.
type ProfileView struct {
	Name            string
	Alpha           float64
	Beta            float64
	FitWeights      Weights
	PackWeights     Weights
	GPUPolicy       GPUPolicy
	GPUAvoidPenalty int64
}

// Validate reports whether the view can be fed to Evaluate.
//
// Startup validation of the configuration already enforces these rules; this
// method exists because Evaluate is a public library entry point that must not
// silently produce a meaningless score for a malformed profile.
func (p ProfileView) Validate() error {
	if p.Alpha < 0 {
		return fmt.Errorf("profile %q: alpha is negative: %v", p.Name, p.Alpha)
	}
	if p.Beta < 0 {
		return fmt.Errorf("profile %q: beta is negative: %v", p.Name, p.Beta)
	}
	if p.Alpha+p.Beta == 0 {
		return fmt.Errorf("profile %q: alpha and beta are both zero", p.Name)
	}
	if err := p.FitWeights.Validate("fit"); err != nil {
		return fmt.Errorf("profile %q: %w", p.Name, err)
	}
	if err := p.PackWeights.Validate("pack"); err != nil {
		return fmt.Errorf("profile %q: %w", p.Name, err)
	}
	if p.FitWeights.IsZero() && p.PackWeights.IsZero() {
		return fmt.Errorf("profile %q: all fit and pack weights are zero", p.Name)
	}
	switch p.GPUPolicy {
	case GPUPolicyAllow, GPUPolicyAvoid:
	default:
		return fmt.Errorf("profile %q: unknown gpu node policy %q", p.Name, p.GPUPolicy)
	}
	if p.GPUAvoidPenalty < 0 || p.GPUAvoidPenalty > 100 {
		return fmt.Errorf("profile %q: gpu avoid penalty out of range [0,100]: %d", p.Name, p.GPUAvoidPenalty)
	}
	return nil
}

// Breakdown is the per-node result of Evaluate. Fit and Pack are kept in their
// natural [0,1] range for explanation, Final is the integer the framework sees.
type Breakdown struct {
	Profile string
	// Fit is the weighted Bray-Curtis structural match in [0,1].
	Fit float64
	// FitSkipped reports that every fit dimension was skipped (zero request),
	// in which case the neutral value Neutral was used.
	FitSkipped bool
	// Pack is the packing score in [0,1].
	Pack float64
	// PackSkipped reports that every pack dimension was skipped.
	PackSkipped bool
	// Combined is alpha*Fit+beta*Pack clamped to [0,1] before rounding.
	Combined float64
	// PrePenalty is Combined rounded to an integer score in [0,100].
	PrePenalty int64
	// GPUPenalty is the penalty actually subtracted (0 when not applicable).
	GPUPenalty int64
	// Final is the score handed to the framework, in [0,100].
	Final int64
}

// ErrGPUNodeInsufficient is returned by Pack when a node has zero allocatable
// capacity for a dimension the Pod requests. This is an anomaly the framework
// should not see (a Filter plugin must have rejected such a node already), so
// it is reported as an error instead of being silently scored.
var ErrGPUNodeInsufficient = errors.New("node allocatable is zero for a requested dimension")

// ErrInvalidNumeric is returned when a scoring component produced NaN or Inf.
var ErrInvalidNumeric = errors.New("scoring produced a non-finite value")
