package config

import (
	"errors"
	"strings"
	"testing"
)

// validYAML is a minimal configuration every case below mutates one rule at a
// time, so a failure points at exactly one §4.3 rule.
const validYAML = `
configVersion: "2026-10-tahrs1"
defaultProfile: cpu-general-v1
profiles:
  cpu-general-v1:
    fitWeights: {cpu: 1, memory: 1}
    packWeights: {cpu: 1, memory: 1}
`

func loadYAML(t *testing.T, body string) (*Loaded, error) {
	t.Helper()
	return Load([]byte(body))
}

// TestValidationRuleHasNegativeCase covers every rule of plan §4.3 with at
// least one configuration the scheduler must refuse to start with.
func TestValidationRuleHasNegativeCase(t *testing.T) {
	cases := []struct {
		name  string
		yaml  string
		wants []string
	}{
		{
			name:  "config_version_missing",
			yaml:  strings.Replace(validYAML, `configVersion: "2026-10-tahrs1"`, "", 1),
			wants: []string{"configVersion must not be empty"},
		},
		{
			name: "alpha_negative",
			yaml: `
configVersion: v1
defaultProfile: p
profiles:
  p: {alpha: -0.1, beta: 0.7, fitWeights: {cpu: 1}}
`,
			wants: []string{"profiles[p].alpha=-0.1 must not be negative"},
		},
		{
			name: "beta_negative",
			yaml: `
configVersion: v1
defaultProfile: p
profiles:
  p: {alpha: 0.3, beta: -2, fitWeights: {cpu: 1}}
`,
			wants: []string{"profiles[p].beta=-2 must not be negative"},
		},
		{
			name: "alpha_plus_beta_zero",
			yaml: `
configVersion: v1
defaultProfile: p
profiles:
  p: {alpha: 0, beta: 0, fitWeights: {cpu: 1}}
`,
			wants: []string{"profiles[p]: alpha+beta must not be 0"},
		},
		{
			name: "weight_negative",
			yaml: `
configVersion: v1
defaultProfile: p
profiles:
  p: {fitWeights: {cpu: -1}}
`,
			wants: []string{`profiles[p].fitWeights.cpu=-1 must not be negative`},
		},
		{
			name: "all_weights_zero",
			yaml: `
configVersion: v1
defaultProfile: p
profiles:
  p: {fitWeights: {cpu: 0}, packWeights: {memory: 0}}
`,
			wants: []string{"profiles[p]: all fitWeights and packWeights are 0"},
		},
		{
			name: "unknown_dimension",
			yaml: `
configVersion: v1
defaultProfile: p
profiles:
  p: {fitWeights: {disk: 1}}
`,
			wants: []string{`profiles[p].fitWeights has unknown dimension "disk"`},
		},
		{
			name: "pack_weight_unknown_dimension",
			yaml: `
configVersion: v1
defaultProfile: p
profiles:
  p: {packWeights: {cpu: 1, GPU: 1}}
`,
			wants: []string{`profiles[p].packWeights has unknown dimension "GPU"`},
		},
		{
			name: "penalty_negative",
			yaml: `
configVersion: v1
gpuAvoidPenalty: -1
defaultProfile: p
profiles:
  p: {fitWeights: {cpu: 1}}
`,
			wants: []string{"gpuAvoidPenalty=-1 must be within [0, 100]"},
		},
		{
			name: "penalty_above_range",
			yaml: `
configVersion: v1
gpuAvoidPenalty: 101
defaultProfile: p
profiles:
  p: {fitWeights: {cpu: 1}}
`,
			wants: []string{"gpuAvoidPenalty=101 must be within [0, 100]"},
		},
		{
			name: "default_profile_empty",
			yaml: `
configVersion: v1
profiles:
  p: {fitWeights: {cpu: 1}}
`,
			wants: []string{"defaultProfile must not be empty"},
		},
		{
			name: "default_profile_undefined",
			yaml: `
configVersion: v1
defaultProfile: missing
profiles:
  p: {fitWeights: {cpu: 1}}
`,
			wants: []string{`defaultProfile="missing" is not defined`},
		},
		{
			name: "policy_invalid",
			yaml: `
configVersion: v1
defaultProfile: p
profiles:
  p: {fitWeights: {cpu: 1}, gpuNodePolicy: prefer}
`,
			wants: []string{`profiles[p].gpuNodePolicy="prefer" must be "allow" or "avoid"`},
		},
		{
			name: "profiles_empty",
			yaml: `
configVersion: v1
defaultProfile: p
profiles: {}
`,
			wants: []string{"profiles must not be empty", `defaultProfile="p" is not defined`},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadYAML(t, tc.yaml)
			if err == nil {
				t.Fatal("Load accepted an invalid configuration")
			}
			var verr *ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("error = %v, want *ValidationError", err)
			}
			for _, want := range tc.wants {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

// TestGPUWeightsRequireResourceName covers the last §4.3 rule. The loader
// defaults gpuResourceName, so this rule can only be hit by callers that build
// a Setting in code; Validate still has to catch it.
func TestGPUWeightsRequireResourceName(t *testing.T) {
	base := Setting{
		ConfigVersion:   "v1",
		GPUResourceName: "",
		DefaultProfile:  "gpu",
		GPUAvoidPenalty: 30,
		Profiles: map[string]Profile{
			"gpu": {
				Alpha:         0.3,
				Beta:          0.7,
				FitWeights:    map[string]float64{"gpu": 3},
				PackWeights:   map[string]float64{"cpu": 1},
				GPUNodePolicy: PolicyAllow,
			},
		},
	}
	if err := base.Validate(); err == nil || !strings.Contains(err.Error(), "gpuResourceName must not be empty") {
		t.Fatalf("error = %v, want the gpuResourceName rule", err)
	}

	// Same setting with an explicit resource name is valid.
	base.GPUResourceName = "nvidia.com/gpu"
	if err := base.Validate(); err != nil {
		t.Fatalf("valid setting rejected: %v", err)
	}
}

// TestValidationAggregatesProblems makes sure one failed start lists every
// mistake instead of only the first one.
func TestValidationAggregatesProblems(t *testing.T) {
	_, err := loadYAML(t, `
configVersion: ""
gpuAvoidPenalty: 500
defaultProfile: p
profiles:
  p:
    fitWeights: {cpu: -1, disk: 2}
    gpuNodePolicy: maybe
`)
	if err == nil {
		t.Fatal("expected a validation error")
	}
	want := []string{
		"configVersion must not be empty",
		"gpuAvoidPenalty=500 must be within [0, 100]",
		`profiles[p].fitWeights.cpu=-1 must not be negative`,
		`profiles[p].fitWeights has unknown dimension "disk"`,
		`profiles[p].gpuNodePolicy="maybe"`,
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("aggregated error misses %q: %v", w, err)
		}
	}
}

func TestValidExamplePassesValidation(t *testing.T) {
	loaded := mustLoadExample(t)
	if err := loaded.Setting.Validate(); err != nil {
		t.Fatalf("example config rejected: %v", err)
	}
	if !loaded.Setting.HasGPUWeight() {
		t.Fatal("example config should use the gpu dimension")
	}
}

// TestPolicyIsCaseInsensitive documents the deliberate leniency: only the
// policy field is normalised, weights and dimensions are not.
func TestPolicyIsCaseInsensitive(t *testing.T) {
	loaded, err := loadYAML(t, `
configVersion: v1
defaultProfile: p
profiles:
  p: {fitWeights: {cpu: 1}, gpuNodePolicy: ALLOW}
`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := loaded.Setting.Profiles["p"].GPUNodePolicy; got != PolicyAllow {
		t.Errorf("policy = %q, want allow", got)
	}
}
