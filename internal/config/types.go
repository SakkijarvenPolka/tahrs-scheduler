// Package config loads, validates and compiles the tahrs-scheduler
// configuration file. Reading files happens once, at startup; the scheduler
// hot path never touches this package.
//
// The package deliberately avoids k8s.io/* imports (see plan §3.2): it only
// needs sigs.k8s.io/yaml, so internal/model stays the single source of truth
// for scoring types.
package config

const (
	// DefaultGPUResourceName is used when gpuResourceName is not set.
	DefaultGPUResourceName = "nvidia.com/gpu"
	// DefaultGPUAvoidPenalty is the soft penalty applied to GPU nodes when a
	// profile prefers to keep them free for GPU workloads.
	DefaultGPUAvoidPenalty int64 = 30
	// DefaultAlpha weighs the Fit component of the final score.
	DefaultAlpha float64 = 0.3
	// DefaultBeta weighs the Pack component of the final score.
	DefaultBeta float64 = 0.7
)

// Setting is the validated, fully defaulted configuration.
type Setting struct {
	ConfigVersion   string             `json:"configVersion"`
	GPUResourceName string             `json:"gpuResourceName"`
	DefaultProfile  string             `json:"defaultProfile"`
	Profiles        map[string]Profile `json:"profiles"`
	GPUAvoidPenalty int64              `json:"gpuAvoidPenalty"`
}

// Profile is a single scoring profile as written in the configuration file.
type Profile struct {
	Alpha         float64            `json:"alpha"`
	Beta          float64            `json:"beta"`
	FitWeights    map[string]float64 `json:"fitWeights"`
	PackWeights   map[string]float64 `json:"packWeights"`
	GPUNodePolicy string             `json:"gpuNodePolicy"`
}

// Loaded pairs a compiled setting with its configuration hash. The hash is part
// of the startup fingerprint and of PreFilter rejection reasons, so operators
// can tell which configuration a scheduler replica is running.
type Loaded struct {
	Setting *Setting
	// Hash is the first 8 hex characters of the SHA-256 of the canonical
	// JSON encoding of Setting.
	Hash string
}

// rawSetting mirrors Setting but keeps pointer fields for values whose zero
// value is meaningful (explicit 0 penalty, explicit 0 alpha) so that defaults
// can be distinguished from explicit zeros.
type rawSetting struct {
	ConfigVersion   string                `json:"configVersion"`
	GPUResourceName string                `json:"gpuResourceName"`
	DefaultProfile  string                `json:"defaultProfile"`
	Profiles        map[string]rawProfile `json:"profiles"`
	GPUAvoidPenalty *int64                `json:"gpuAvoidPenalty"`
}

type rawProfile struct {
	Alpha         *float64           `json:"alpha"`
	Beta          *float64           `json:"beta"`
	FitWeights    map[string]float64 `json:"fitWeights"`
	PackWeights   map[string]float64 `json:"packWeights"`
	GPUNodePolicy string             `json:"gpuNodePolicy"`
}

// toSetting applies defaults and normalises nil maps so that the canonical hash
// does not depend on whether a writer omitted an empty map.
func (r *rawSetting) toSetting() *Setting {
	s := &Setting{
		ConfigVersion:   r.ConfigVersion,
		GPUResourceName: r.GPUResourceName,
		DefaultProfile:  r.DefaultProfile,
		Profiles:        make(map[string]Profile, len(r.Profiles)),
		GPUAvoidPenalty: DefaultGPUAvoidPenalty,
	}
	if r.GPUAvoidPenalty != nil {
		s.GPUAvoidPenalty = *r.GPUAvoidPenalty
	}
	if s.GPUResourceName == "" {
		s.GPUResourceName = DefaultGPUResourceName
	}
	for name, rp := range r.Profiles {
		p := Profile{
			Alpha:         DefaultAlpha,
			Beta:          DefaultBeta,
			FitWeights:    normaliseWeights(rp.FitWeights),
			PackWeights:   normaliseWeights(rp.PackWeights),
			GPUNodePolicy: normalisePolicy(rp.GPUNodePolicy),
		}
		if rp.Alpha != nil {
			p.Alpha = *rp.Alpha
		}
		if rp.Beta != nil {
			p.Beta = *rp.Beta
		}
		s.Profiles[name] = p
	}
	return s
}

func normaliseWeights(in map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// normalisePolicy lower-cases the policy and defaults to the conservative
// "avoid" so a missing field never silently risks disturbing GPU nodes.
func normalisePolicy(policy string) string {
	if policy == "" {
		return "avoid"
	}
	if policy == "ALLOW" || policy == "allow" {
		return "allow"
	}
	if policy == "AVOID" || policy == "avoid" {
		return "avoid"
	}
	return policy
}
