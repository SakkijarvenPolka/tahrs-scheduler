package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
)

// LoadFile reads and validates the configuration file at path.
//
// It is called once per process, during scheduler construction, so plain file
// I/O is acceptable here; the Score hot path never calls into this package.
func LoadFile(path string) (*Loaded, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %q: %w", path, err)
	}
	loaded, err := Load(data)
	if err != nil {
		return nil, fmt.Errorf("config: %q: %w", path, err)
	}
	return loaded, nil
}

// Load parses, defaults and validates configuration bytes.
func Load(data []byte) (*Loaded, error) {
	var raw rawSetting
	if err := yaml.UnmarshalStrict(data, &raw); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	setting := raw.toSetting()
	if err := setting.Validate(); err != nil {
		return nil, err
	}
	hash, err := setting.Hash()
	if err != nil {
		return nil, err
	}
	return &Loaded{Setting: setting, Hash: hash}, nil
}

// Hash returns the first 8 hex characters of the SHA-256 of the canonical JSON
// encoding of the setting. encoding/json sorts map keys and the loader
// normalises nil maps, so equal configurations hash equally.
func (s *Setting) Hash() (string, error) {
	canonical, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("hash: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])[:8], nil
}

// Fingerprint is the single line printed at startup (plan §4.3): configuration
// version, hash, the profiles with their parameters and the build identifier.
//
// The output is stable: profile names are sorted and numbers are formatted with
// fixed precision, so log diffing across restarts stays useful.
func (l *Loaded) Fingerprint(build string) string {
	s := l.Setting
	var b strings.Builder
	fmt.Fprintf(&b, "configVersion=%s configHash=%s build=%s defaultProfile=%s gpuResource=%s gpuAvoidPenalty=%d",
		s.ConfigVersion, l.Hash, build, s.DefaultProfile, s.GPUResourceName, s.GPUAvoidPenalty)
	names := make([]string, 0, len(s.Profiles))
	for name := range s.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	b.WriteString(" profiles=[")
	for i, name := range names {
		if i > 0 {
			b.WriteString(" ")
		}
		p := s.Profiles[name]
		fmt.Fprintf(&b, "%s{alpha=%.2f beta=%.2f fit=%s pack=%s policy=%s}",
			name, p.Alpha, p.Beta, formatWeights(p.FitWeights), formatWeights(p.PackWeights), p.GPUNodePolicy)
	}
	b.WriteString("]")
	return b.String()
}

// formatWeights renders cpu/memory/gpu in a fixed order, skipping absent
// dimensions, so the fingerprint does not depend on map iteration order.
func formatWeights(w map[string]float64) string {
	var parts []string
	for _, dim := range []string{"cpu", "memory", "gpu"} {
		v, ok := w[dim]
		if !ok {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=%.2f", dim, v))
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, ",")
}
