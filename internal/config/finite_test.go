package config

import (
	"math"
	"strings"
	"testing"
)

// finiteBaseSetting is a configuration that passes Validate. Each case below
// breaks exactly one numeric rule of plan §4.3.
func finiteBaseSetting() Setting {
	return Setting{
		ConfigVersion:   "test-2026-10",
		GPUResourceName: DefaultGPUResourceName,
		DefaultProfile:  "cpu-general-v1",
		GPUAvoidPenalty: 30,
		Profiles: map[string]Profile{
			"cpu-general-v1": {
				Alpha:         0.3,
				Beta:          0.7,
				FitWeights:    map[string]float64{DimCPU: 1, DimMemory: 1},
				PackWeights:   map[string]float64{DimCPU: 1, DimMemory: 1},
				GPUNodePolicy: PolicyAvoid,
			},
		},
	}
}

func TestValidateAcceptsFiniteSetting(t *testing.T) {
	setting := finiteBaseSetting()
	if err := setting.Validate(); err != nil {
		t.Fatalf("valid setting rejected: %v", err)
	}
}

// TestValidateRejectsNonFiniteNumbers pins the fix for the NaN/Inf hole: every
// comparison against NaN is false, so a NaN alpha used to pass validation, turn
// the combined score into NaN and only surface as a broken integer score.
func TestValidateRejectsNonFiniteNumbers(t *testing.T) {
	nan := math.NaN()
	inf := math.Inf(1)
	negInf := math.Inf(-1)

	cases := []struct {
		name   string
		mutate func(*Profile)
		want   string
	}{
		{name: "alpha NaN", mutate: func(p *Profile) { p.Alpha = nan }, want: "alpha=NaN must be a finite number"},
		{name: "beta NaN", mutate: func(p *Profile) { p.Beta = nan }, want: "beta=NaN must be a finite number"},
		{name: "alpha +Inf", mutate: func(p *Profile) { p.Alpha = inf }, want: "alpha=+Inf must be a finite number"},
		{name: "beta -Inf", mutate: func(p *Profile) { p.Beta = negInf }, want: "beta=-Inf must be a finite number"},
		{name: "fit weight NaN", mutate: func(p *Profile) {
			p.FitWeights[DimMemory] = nan
		}, want: "fitWeights.memory=NaN must be a finite number"},
		{name: "pack weight +Inf", mutate: func(p *Profile) {
			p.PackWeights[DimCPU] = inf
		}, want: "packWeights.cpu=+Inf must be a finite number"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setting := finiteBaseSetting()
			profile := setting.Profiles["cpu-general-v1"]
			tc.mutate(&profile)
			setting.Profiles["cpu-general-v1"] = profile

			err := setting.Validate()
			if err == nil {
				t.Fatalf("Validate accepted a non-finite %s", tc.name)
			}
			if _, ok := err.(*ValidationError); !ok {
				t.Fatalf("error type = %T, want *ValidationError", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err.Error(), tc.want)
			}
		})
	}
}

// TestValidateReportsEveryProblem keeps the aggregated error aggregated: a file
// with three mistakes must report three, not stop at the first.
func TestValidateReportsEveryProblem(t *testing.T) {
	setting := finiteBaseSetting()
	profile := setting.Profiles["cpu-general-v1"]
	profile.Alpha = math.NaN()
	profile.PackWeights[DimGPU] = -1
	profile.GPUNodePolicy = "maybe"
	setting.Profiles["cpu-general-v1"] = profile
	setting.DefaultProfile = "missing"

	err := setting.Validate()
	if err == nil {
		t.Fatal("Validate accepted an invalid setting")
	}
	validationErr, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("error type = %T, want *ValidationError", err)
	}
	if len(validationErr.Problems) < 4 {
		t.Fatalf("reported %d problems, want at least 4: %v", len(validationErr.Problems), validationErr.Problems)
	}
}
