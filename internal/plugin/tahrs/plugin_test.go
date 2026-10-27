package tahrs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	fwk "k8s.io/kube-scheduler/framework"
	schedframework "k8s.io/kubernetes/pkg/scheduler/framework"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	"sigs.k8s.io/yaml"

	"github.com/SakkijarvenPolka/tahrs-scheduler/internal/config"
	"github.com/SakkijarvenPolka/tahrs-scheduler/internal/kubeadapter"
	"github.com/SakkijarvenPolka/tahrs-scheduler/internal/model"
	"github.com/SakkijarvenPolka/tahrs-scheduler/internal/observability"
	"github.com/SakkijarvenPolka/tahrs-scheduler/internal/profile"
	"github.com/SakkijarvenPolka/tahrs-scheduler/internal/scoring"
)

const gpuResource = "nvidia.com/gpu"

func cpuSetting() config.Setting {
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

func writeConfigYAML(t *testing.T, setting config.Setting) string {
	t.Helper()
	data, err := yaml.Marshal(setting)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	path := filepath.Join(t.TempDir(), "tahrs-config.yaml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// TestDecodeArgs covers the two shapes the scheduler can hand a plugin factory:
// the typed object (tests, programmatic profiles) and the runtime.Unknown the
// scheduler produces when it decodes pluginConfig args from --config.
func TestDecodeArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    apiruntime.Object
		want    string
		wantErr string
	}{
		{name: "nil args", args: nil},
		{name: "typed args", args: &Args{ConfigPath: "/etc/tahrs/config.yaml"}, want: "/etc/tahrs/config.yaml"},
		{name: "typed nil", args: (*Args)(nil)},
		{name: "unknown object", args: &apiruntime.Unknown{
			ContentType: "application/json",
			Raw:         []byte(`{"configPath":"/etc/tahrs/config.yaml"}`),
		}, want: "/etc/tahrs/config.yaml"},
		{name: "unknown without raw", args: &apiruntime.Unknown{}},
		{name: "unknown with malformed json", args: &apiruntime.Unknown{Raw: []byte("{not json")}, wantErr: "decode plugin args"},
		{name: "unsupported type", args: &v1.Pod{}, wantErr: "unsupported plugin args type"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeArgs(tc.args)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("decodeArgs: %v", err)
			}
			if got == nil {
				t.Fatal("decodeArgs returned nil args without an error")
			}
			if got.ConfigPath != tc.want {
				t.Fatalf("configPath = %q, want %q", got.ConfigPath, tc.want)
			}
		})
	}
}

func TestNewRequiresConfigPath(t *testing.T) {
	t.Setenv(ConfigPathEnv, "")

	if _, err := New(context.Background(), nil, nil); err == nil || !strings.Contains(err.Error(), ConfigPathEnv) {
		t.Fatalf("error = %v, want one mentioning %s", err, ConfigPathEnv)
	}
}

func TestNewRejectsMissingFile(t *testing.T) {
	t.Setenv(ConfigPathEnv, "")
	missing := filepath.Join(t.TempDir(), "absent.yaml")

	_, err := New(context.Background(), &Args{ConfigPath: missing}, nil)
	if err == nil || !strings.Contains(err.Error(), "tahrs:") {
		t.Fatalf("error = %v, want one wrapping the load failure", err)
	}
}

func TestNewRejectsInvalidConfiguration(t *testing.T) {
	setting := cpuSetting()
	setting.Profiles["cpu-general-v1"] = config.Profile{
		Alpha:      0.3,
		Beta:       0.7,
		FitWeights: map[string]float64{"cache": 1}, // unknown dimension
	}
	path := writeConfigYAML(t, setting)

	if _, err := New(context.Background(), &Args{ConfigPath: path}, nil); err == nil {
		t.Fatal("New accepted a configuration with an unknown dimension")
	}
}

// TestNewFromRuntimeUnknown is the end-to-end shape: --config carries
// pluginConfig args that arrive as *runtime.Unknown.
func TestNewFromRuntimeUnknown(t *testing.T) {
	setting := cpuSetting()
	path := writeConfigYAML(t, setting)
	t.Setenv(ConfigPathEnv, "")

	args := &apiruntime.Unknown{
		ContentType: "application/json",
		Raw:         []byte(`{"configPath":"` + path + `"}`),
	}
	plugin, err := New(context.Background(), args, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	p, ok := plugin.(*Plugin)
	if !ok {
		t.Fatalf("New returned %T, want *Plugin", plugin)
	}
	if got := p.Name(); got != Name {
		t.Fatalf("Name() = %q, want %q", got, Name)
	}
	if len(p.hash) != 8 {
		t.Fatalf("config hash = %q, want 8 hex characters", p.hash)
	}
	if len(p.views) != len(setting.Profiles) {
		t.Fatalf("compiled %d profiles, want %d", len(p.views), len(setting.Profiles))
	}
}

func TestNewFallsBackToEnv(t *testing.T) {
	path := writeConfigYAML(t, cpuSetting())
	t.Setenv(ConfigPathEnv, path)

	if _, err := New(context.Background(), nil, nil); err != nil {
		t.Fatalf("New with %s set: %v", ConfigPathEnv, err)
	}
}

func newTestPlugin(t *testing.T, setting config.Setting) *Plugin {
	t.Helper()
	loaded, err := config.Load(mustMarshal(t, setting))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	views, err := loaded.Setting.Compile()
	if err != nil {
		t.Fatalf("compile config: %v", err)
	}
	return &Plugin{
		setting: loaded.Setting,
		views:   views,
		hash:    loaded.Hash,
		metrics: observability.New(),
	}
}

func mustMarshal(t *testing.T, setting config.Setting) []byte {
	t.Helper()
	data, err := yaml.Marshal(setting)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	return data
}

// TestPreScoreMissingStateIsAnError: a missing CycleState means PreFilter never
// ran, which is an internal error rather than a scoring anomaly.
func TestPreScoreMissingStateIsAnError(t *testing.T) {
	p := newTestPlugin(t, cpuSetting())
	state := schedframework.NewCycleState()

	status := p.PreScore(context.Background(), state, &v1.Pod{}, nil)
	if status == nil || status.Code() != fwk.Error {
		t.Fatalf("PreScore status = %v, want an Error status", status)
	}
}

// nodeInfo adapts a v1.Node into the interface Score receives.
func nodeInfo(node *v1.Node) fwk.NodeInfo {
	info := schedframework.NewNodeInfo()
	info.SetNode(node)
	return info
}

// TestScoreMissingStateReturnsMinScore: R7, Score must never fail the Pod, even
// when PreFilter did not run.
func TestScoreMissingStateReturnsMinScore(t *testing.T) {
	p := newTestPlugin(t, cpuSetting())
	state := schedframework.NewCycleState()
	n := st.MakeNode().Name("node-a").Capacity(map[v1.ResourceName]string{
		v1.ResourceCPU:    "8000m",
		v1.ResourceMemory: "32Gi",
	}).Obj()

	score, status := p.Score(context.Background(), state, &v1.Pod{}, nodeInfo(n))
	if status != nil {
		t.Fatalf("Score status = %v, want nil", status)
	}
	if score != fwk.MinScore {
		t.Fatalf("Score = %d, want MinScore %d", score, fwk.MinScore)
	}
}

// TestScoreUsesCycleState checks Score against a direct evaluation of the same
// numbers, which is the property replay relies on.
func TestScoreUsesCycleState(t *testing.T) {
	p := newTestPlugin(t, cpuSetting())
	state := schedframework.NewCycleState()
	n := st.MakeNode().Name("node-a").Capacity(map[v1.ResourceName]string{
		v1.ResourceCPU:    "8000m",
		v1.ResourceMemory: "32Gi",
	}).Obj()
	pod := st.MakePod().Name("workload").Namespace("default").Req(map[v1.ResourceName]string{
		v1.ResourceCPU:    "500m",
		v1.ResourceMemory: "1Gi",
	}).Obj()

	resolution, err := profile.Resolve(nil, p.setting.DefaultProfile, p.views)
	if err != nil {
		t.Fatalf("resolve profile: %v", err)
	}
	state.Write(stateKey, &stateData{
		Resolution: resolution,
		Requests:   kubeadapter.PodRequests(pod, v1.ResourceName(p.setting.GPUResourceName)),
	})

	score, status := p.Score(context.Background(), state, pod, nodeInfo(n))
	if status != nil {
		t.Fatalf("Score status = %v, want nil", status)
	}

	// The node has no resident pods, so requested is zero.
	want, err := scoring.Evaluate(
		kubeadapter.PodRequests(pod, v1.ResourceName(p.setting.GPUResourceName)),
		model.NodeSnapshot{
			Name:        "node-a",
			Allocatable: model.Resources{MilliCPU: 8000, MemoryBytes: 32 << 30},
		},
		resolution.Profile,
	)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if score != want.Final {
		t.Fatalf("Score = %d, direct evaluation = %d", score, want.Final)
	}
	if score <= fwk.MinScore || score > fwk.MaxScore {
		t.Fatalf("Score = %d, want a value in (%d, %d]", score, fwk.MinScore, fwk.MaxScore)
	}
}

// TestScoreAnomalyReturnsMinScore: a GPU Pod on a node without GPU capacity must
// be scored MinScore with a nil status (R7).
func TestScoreAnomalyReturnsMinScore(t *testing.T) {
	setting := cpuSetting()
	setting.DefaultProfile = "cpu-general-v1"
	p := newTestPlugin(t, setting)
	state := schedframework.NewCycleState()
	n := st.MakeNode().Name("cpu-only").Capacity(map[v1.ResourceName]string{
		v1.ResourceCPU:    "8000m",
		v1.ResourceMemory: "32Gi",
	}).Obj()
	pod := st.MakePod().Name("gpu-workload").Namespace("default").Req(map[v1.ResourceName]string{
		v1.ResourceCPU:               "500m",
		v1.ResourceMemory:            "1Gi",
		v1.ResourceName(gpuResource): "1",
	}).Obj()

	resolution, err := profile.Resolve(map[string]string{profile.AnnotationKey: "gpu-inference-v1"}, setting.DefaultProfile, p.views)
	if err != nil {
		t.Fatalf("resolve profile: %v", err)
	}
	state.Write(stateKey, &stateData{
		Resolution: resolution,
		Requests:   model.TaskRequests{MilliCPU: 500, MemoryBytes: 1 << 30, GPUs: 1},
	})

	score, status := p.Score(context.Background(), state, pod, nodeInfo(n))
	if status != nil {
		t.Fatalf("Score status = %v, want nil", status)
	}
	if score != fwk.MinScore {
		t.Fatalf("Score = %d, want MinScore %d", score, fwk.MinScore)
	}
}

func TestNormalizeScorePassesThrough(t *testing.T) {
	p := newTestPlugin(t, cpuSetting())
	state := schedframework.NewCycleState()
	scores := fwk.NodeScoreList{{Name: "node-a", Score: 40}, {Name: "node-b", Score: 90}}

	if status := p.NormalizeScore(context.Background(), state, &v1.Pod{}, scores); status != nil {
		t.Fatalf("NormalizeScore status = %v, want nil", status)
	}
	if scores[0].Score != 40 || scores[1].Score != 90 {
		t.Fatalf("scores were modified: %v", scores)
	}
}

func TestScoreExtensionsReturnsSelf(t *testing.T) {
	p := newTestPlugin(t, cpuSetting())
	if got := p.ScoreExtensions(); got != p {
		t.Fatalf("ScoreExtensions() = %T, want the plugin itself", got)
	}
}
