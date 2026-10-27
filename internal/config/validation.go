package config

import (
	"fmt"
	"sort"
	"strings"
)

// Dimensions accepted in fitWeights/packWeights.
const (
	DimCPU    = "cpu"
	DimMemory = "memory"
	DimGPU    = "gpu"
)

// GPU node policies.
const (
	PolicyAllow = "allow"
	PolicyAvoid = "avoid"
)

// ValidationError aggregates every problem found in a configuration file, so a
// single failed start lists all mistakes instead of only the first one.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	return "invalid configuration: " + strings.Join(e.Problems, "; ")
}

// Validate enforces plan §4.3. Every rule listed there is checked here; the
// scheduler refuses to start when any of them fails.
func (s *Setting) Validate() error {
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	if s.ConfigVersion == "" {
		add("configVersion must not be empty")
	}
	if s.GPUAvoidPenalty < 0 || s.GPUAvoidPenalty > 100 {
		add("gpuAvoidPenalty=%d must be within [0, 100]", s.GPUAvoidPenalty)
	}
	if len(s.Profiles) == 0 {
		add("profiles must not be empty")
	}
	if s.DefaultProfile == "" {
		add("defaultProfile must not be empty")
	} else if _, ok := s.Profiles[s.DefaultProfile]; !ok {
		add("defaultProfile=%q is not defined in profiles %v", s.DefaultProfile, sortedNames(s.Profiles))
	}
	// The loader defaults GPUResourceName, so the §4.3 rule "gpu weights but no
	// resource name" cannot trigger from a file; the check still guards callers
	// that build a Setting programmatically.
	if s.GPUResourceName == "" {
		if s.HasGPUWeight() {
			add("gpuResourceName must not be empty when %q weights are used", DimGPU)
		} else {
			add("gpuResourceName must not be empty")
		}
	}

	names := make([]string, 0, len(s.Profiles))
	for name := range s.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		p := s.Profiles[name]
		switch {
		case p.Alpha < 0:
			add("profiles[%s].alpha=%v must not be negative", name, p.Alpha)
		case p.Beta < 0:
			add("profiles[%s].beta=%v must not be negative", name, p.Beta)
		case p.Alpha+p.Beta == 0:
			add("profiles[%s]: alpha+beta must not be 0", name)
		}
		if !isPolicy(p.GPUNodePolicy) {
			add("profiles[%s].gpuNodePolicy=%q must be %q or %q", name, p.GPUNodePolicy, PolicyAllow, PolicyAvoid)
		}
		problems = append(problems, validateWeights(name, "fitWeights", p.FitWeights)...)
		problems = append(problems, validateWeights(name, "packWeights", p.PackWeights)...)
		if isZeroWeights(p.FitWeights) && isZeroWeights(p.PackWeights) {
			add("profiles[%s]: all fitWeights and packWeights are 0", name)
		}
	}

	if len(problems) == 0 {
		return nil
	}
	return &ValidationError{Problems: problems}
}

func isPolicy(policy string) bool {
	return policy == PolicyAllow || policy == PolicyAvoid
}

func validateWeights(profile, field string, weights map[string]float64) []string {
	var problems []string
	keys := make([]string, 0, len(weights))
	for k := range weights {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch k {
		case DimCPU, DimMemory, DimGPU:
			if weights[k] < 0 {
				problems = append(problems, fmt.Sprintf("profiles[%s].%s.%s=%v must not be negative", profile, field, k, weights[k]))
			}
		default:
			problems = append(problems, fmt.Sprintf("profiles[%s].%s has unknown dimension %q (want %s, %s or %s)", profile, field, k, DimCPU, DimMemory, DimGPU))
		}
	}
	return problems
}

func isZeroWeights(weights map[string]float64) bool {
	for _, v := range weights {
		if v != 0 {
			return false
		}
	}
	return true
}

func sortedNames(profiles map[string]Profile) []string {
	names := make([]string, 0, len(profiles))
	for name := range profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// HasGPUWeight reports whether either weight vector touches the GPU dimension.
func (s *Setting) HasGPUWeight() bool {
	for _, p := range s.Profiles {
		if p.FitWeights[DimGPU] > 0 || p.PackWeights[DimGPU] > 0 {
			return true
		}
	}
	return false
}
