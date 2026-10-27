package config

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

var update = flag.Bool("update", false, "rewrite testdata/fingerprint.golden")

func examplePath(t *testing.T) string {
	t.Helper()
	return filepath.Join("testdata", "example.yaml")
}

func mustLoadExample(t *testing.T) *Loaded {
	t.Helper()
	loaded, err := LoadFile(examplePath(t))
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	return loaded
}

func TestLoadExampleAppliesDefaults(t *testing.T) {
	loaded := mustLoadExample(t)
	s := loaded.Setting

	if s.ConfigVersion != "2026-10-tahrs1" {
		t.Errorf("configVersion = %q", s.ConfigVersion)
	}
	if s.GPUResourceName != "nvidia.com/gpu" {
		t.Errorf("gpuResourceName = %q", s.GPUResourceName)
	}
	if s.DefaultProfile != "cpu-general-v1" {
		t.Errorf("defaultProfile = %q", s.DefaultProfile)
	}
	if s.GPUAvoidPenalty != 30 {
		t.Errorf("gpuAvoidPenalty = %d, want 30", s.GPUAvoidPenalty)
	}
	if len(s.Profiles) != 2 {
		t.Fatalf("profiles = %d, want 2", len(s.Profiles))
	}

	cpu := s.Profiles["cpu-general-v1"]
	if cpu.Alpha != 0.3 || cpu.Beta != 0.7 {
		t.Errorf("alpha/beta = %v/%v, want 0.3/0.7", cpu.Alpha, cpu.Beta)
	}
	if cpu.GPUNodePolicy != PolicyAvoid {
		t.Errorf("cpu policy = %q, want avoid", cpu.GPUNodePolicy)
	}
	gpu := s.Profiles["gpu-inference-v1"]
	if got := gpu.FitWeights["gpu"]; got != 3 {
		t.Errorf("gpu fit weight = %v, want 3", got)
	}
	if got := gpu.PackWeights["gpu"]; got != 2 {
		t.Errorf("gpu pack weight = %v, want 2", got)
	}
	if len(loaded.Hash) != 8 {
		t.Errorf("hash = %q, want 8 hex characters", loaded.Hash)
	}
}

func TestLoadAppliesDefaultsToSparseProfile(t *testing.T) {
	loaded, err := Load([]byte(`
configVersion: v1
defaultProfile: minimal
profiles:
  minimal:
    fitWeights: {cpu: 1}
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p := loaded.Setting.Profiles["minimal"]
	if p.Alpha != DefaultAlpha || p.Beta != DefaultBeta {
		t.Errorf("alpha/beta = %v/%v, want defaults %v/%v", p.Alpha, p.Beta, DefaultAlpha, DefaultBeta)
	}
	if p.GPUNodePolicy != PolicyAvoid {
		t.Errorf("policy = %q, want the conservative avoid default", p.GPUNodePolicy)
	}
	if loaded.Setting.GPUAvoidPenalty != DefaultGPUAvoidPenalty {
		t.Errorf("penalty = %d, want default %d", loaded.Setting.GPUAvoidPenalty, DefaultGPUAvoidPenalty)
	}
}

func TestLoadKeepsExplicitZeroPenalty(t *testing.T) {
	loaded, err := Load([]byte(`
configVersion: v1
gpuAvoidPenalty: 0
defaultProfile: p
profiles:
  p:
    fitWeights: {cpu: 1}
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Setting.GPUAvoidPenalty != 0 {
		t.Errorf("penalty = %d, want the explicit 0 to survive defaulting", loaded.Setting.GPUAvoidPenalty)
	}
}

func TestLoadRejectsUnknownField(t *testing.T) {
	_, err := Load([]byte(`
configVersion: v1
defaultProfile: p
unknownField: true
profiles:
  p:
    fitWeights: {cpu: 1}
`))
	if err == nil {
		t.Fatal("Load accepted an unknown field")
	}
	if !strings.Contains(err.Error(), "unknownField") {
		t.Errorf("error %q does not name the unknown field", err)
	}
}

func TestLoadRejectsMalformedYAML(t *testing.T) {
	if _, err := Load([]byte("profiles: [\n")); err == nil {
		t.Fatal("Load accepted malformed YAML")
	}
}

func TestHashIsIndependentOfFormattingAndEmptyMaps(t *testing.T) {
	compact := `
configVersion: v1
defaultProfile: p
profiles:
  p:
    fitWeights: {cpu: 1, memory: 1}
`
	// Same configuration: reordered keys, explicit empty packWeights, excessive
	// whitespace. The canonical hash must not change.
	spread := `
configVersion:   "v1"
defaultProfile:  p
profiles:
  p:
    packWeights: {}
    fitWeights:
      memory: 1
      cpu: 1
`
	a, err := Load([]byte(compact))
	if err != nil {
		t.Fatalf("Load compact: %v", err)
	}
	b, err := Load([]byte(spread))
	if err != nil {
		t.Fatalf("Load spread: %v", err)
	}
	if a.Hash != b.Hash {
		t.Fatalf("hash depends on formatting: %s != %s", a.Hash, b.Hash)
	}
	if !strings.Contains(b.Fingerprint("dev"), "pack=-") {
		t.Errorf("empty pack weights should render as '-': %s", b.Fingerprint("dev"))
	}
}

func TestHashChangesWhenScoringChanges(t *testing.T) {
	base := mustLoadExample(t)
	changed := *base.Setting
	changed.GPUAvoidPenalty = base.Setting.GPUAvoidPenalty + 1
	hash, err := changed.Hash()
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if hash == base.Hash {
		t.Fatalf("hash %s did not change although the penalty did", hash)
	}
}

// TestFingerprintGolden pins the single startup line (plan §4.3). Run
// `go test ./internal/config -update` after an intentional format change.
func TestFingerprintGolden(t *testing.T) {
	loaded := mustLoadExample(t)
	got := loaded.Fingerprint("v0.1.0-test")

	const goldenPath = "testdata/fingerprint.golden"
	if *update {
		if err := os.WriteFile(goldenPath, []byte(got+"\n"), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if got != strings.TrimSuffix(string(want), "\n") {
		t.Fatalf("fingerprint changed:\n got: %s\nwant: %s", got, strings.TrimSuffix(string(want), "\n"))
	}
	if !strings.Contains(got, "configHash="+loaded.Hash) {
		t.Errorf("fingerprint does not carry the config hash: %s", got)
	}
	// Sorted profile names keep log diffing readable.
	if strings.Index(got, "cpu-general-v1{") > strings.Index(got, "gpu-inference-v1{") {
		t.Errorf("profiles are not sorted: %s", got)
	}
}

func TestLoadFileErrorsAreWrapped(t *testing.T) {
	_, err := LoadFile(filepath.Join(t.TempDir(), "absent.yaml"))
	if err == nil {
		t.Fatal("LoadFile accepted a missing file")
	}
	if !strings.Contains(err.Error(), "config: read") {
		t.Errorf("error %q lost the read context", err)
	}

	bad := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(bad, []byte("configVersion: \"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = LoadFile(bad)
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("error = %v, want a *ValidationError", err)
	}
	if !strings.Contains(err.Error(), bad) {
		t.Errorf("error %q does not name the file", err)
	}
}

func TestLoadExampleMatchesPlanExample(t *testing.T) {
	// Guards against the fixture and the plan drifting apart: the file is the
	// plan's §4.2 example verbatim, so a YAML round-trip must be stable.
	raw, err := os.ReadFile(examplePath(t))
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(raw)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	roundTrip, err := yaml.Marshal(loaded.Setting)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(roundTrip)
	if err != nil {
		t.Fatalf("reload marshalled setting: %v", err)
	}
	if reloaded.Hash != loaded.Hash {
		t.Fatalf("hash changed after a YAML round-trip: %s != %s", reloaded.Hash, loaded.Hash)
	}
}
