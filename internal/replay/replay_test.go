package replay

import (
	"bytes"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/SakkijarvenPolka/tahrs-scheduler/internal/config"
	"github.com/SakkijarvenPolka/tahrs-scheduler/internal/scoring"
)

const gpuResource = "nvidia.com/gpu"

func testSetting() config.Setting {
	return config.Setting{
		ConfigVersion:   "unit-test",
		GPUResourceName: gpuResource,
		DefaultProfile:  "cpu-general-v1",
		GPUAvoidPenalty: 30,
		Profiles: map[string]config.Profile{
			"cpu-general-v1": {
				Alpha:         0.3,
				Beta:          0.7,
				FitWeights:    map[string]float64{"cpu": 1, "memory": 1},
				PackWeights:   map[string]float64{"cpu": 1, "memory": 1},
				GPUNodePolicy: "avoid",
			},
			"gpu-inference-v1": {
				Alpha:         0.3,
				Beta:          0.7,
				FitWeights:    map[string]float64{"cpu": 1, "memory": 1, "gpu": 3},
				PackWeights:   map[string]float64{"cpu": 1, "memory": 1, "gpu": 2},
				GPUNodePolicy: "allow",
			},
		},
	}
}

func loadSetting(t *testing.T, setting config.Setting) *config.Loaded {
	t.Helper()
	data, err := yaml.Marshal(setting)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	loaded, err := config.Load(data)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return loaded
}

func TestTableRanksNodesByScore(t *testing.T) {
	loaded := loadSetting(t, testSetting())
	cluster := Cluster{Nodes: []Node{
		{Name: "wide", Allocatable: map[string]string{"cpu": "32000m", "memory": "128Gi"}},
		{Name: "narrow", Allocatable: map[string]string{"cpu": "4000m", "memory": "8Gi"}},
		{Name: "empty", Allocatable: map[string]string{}},
	}}
	workload := Workload{
		Name:     "web",
		Requests: map[string]string{"cpu": "500m", "memory": "1Gi"},
	}

	result, err := Table(loaded, workload, cluster)
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	if result.ConfigHash != loaded.Hash {
		t.Errorf("config hash = %q, want %q", result.ConfigHash, loaded.Hash)
	}
	if result.Profile != "cpu-general-v1" {
		t.Errorf("profile = %q, want the default cpu-general-v1", result.Profile)
	}
	if len(result.Rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(result.Rows))
	}

	// Zero allocatable is an anomaly, not something to skip silently.
	var empty *Row
	for i := range result.Rows {
		if result.Rows[i].Node == "empty" {
			empty = &result.Rows[i]
		}
	}
	if empty == nil {
		t.Fatal("the node with no allocatable resources is missing from the table")
	}
	if empty.Err == "" || empty.Score != scoring.MinScore {
		t.Fatalf("empty node row = %+v, want MinScore and an anomaly", *empty)
	}

	for i := 1; i < len(result.Rows); i++ {
		prev, cur := result.Rows[i-1], result.Rows[i]
		if prev.Score < cur.Score {
			t.Fatalf("rows are not sorted by score: %v", result.Rows)
		}
		if prev.Score == cur.Score && prev.Node > cur.Node {
			t.Fatalf("equal scores are not tie-broken by node name: %v", result.Rows)
		}
	}
}

func TestTableUsesWorkloadProfile(t *testing.T) {
	loaded := loadSetting(t, testSetting())
	cluster := Cluster{Nodes: []Node{
		{Name: "gpu", Allocatable: map[string]string{"cpu": "8000m", "memory": "32Gi", gpuResource: "4"}},
	}}
	workload := Workload{
		Name:     "inference",
		Profile:  "gpu-inference-v1",
		Requests: map[string]string{"cpu": "1000m", "memory": "2Gi", gpuResource: "1"},
	}

	result, err := Table(loaded, workload, cluster)
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	if result.Profile != "gpu-inference-v1" {
		t.Fatalf("profile = %q, want gpu-inference-v1", result.Profile)
	}
	if len(result.Rows) != 1 || result.Rows[0].Err != "" {
		t.Fatalf("rows = %+v, want one clean row", result.Rows)
	}
}

func TestTableRejectsUnknownProfile(t *testing.T) {
	loaded := loadSetting(t, testSetting())
	workload := Workload{Name: "web", Profile: "no-such-profile"}

	_, err := Table(loaded, workload, Cluster{})
	if err == nil {
		t.Fatal("Table accepted an unknown profile")
	}
	for _, want := range []string{"no-such-profile", "cpu-general-v1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestTableRejectsBadQuantity(t *testing.T) {
	loaded := loadSetting(t, testSetting())
	cluster := Cluster{Nodes: []Node{{Name: "node-a", Allocatable: map[string]string{"cpu": "not-a-quantity"}}}}

	if _, err := Table(loaded, Workload{Name: "web"}, cluster); err == nil || !strings.Contains(err.Error(), "node-a") {
		t.Fatalf("error = %v, want one naming the node", err)
	}
}

func TestPrintIncludesNotes(t *testing.T) {
	loaded := loadSetting(t, testSetting())
	cluster := Cluster{Nodes: []Node{
		{Name: "gpu", Allocatable: map[string]string{"cpu": "8000m", "memory": "32Gi", gpuResource: "4"}},
		{Name: "cpu", Allocatable: map[string]string{"cpu": "8000m", "memory": "32Gi"}},
	}}
	workload := Workload{
		Profile:  "gpu-inference-v1",
		Requests: map[string]string{"cpu": "1000m", "memory": "2Gi", gpuResource: "1"},
	}

	result, err := Table(loaded, workload, cluster)
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	var buf bytes.Buffer
	if err := Print(&buf, result); err != nil {
		t.Fatalf("Print: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"profile=gpu-inference-v1", "NODE", "FIT", "PACK", "PENALTY", "SCORE", "NOTE", "error:"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q:\n%s", want, out)
		}
	}
}
