// Package framework_test runs the Tahrs plugin inside the real Kubernetes
// scheduling framework: the same runtime, the same extension-point ordering and
// the same native plugins a cluster would run. The tests in this directory are
// the T3 acceptance criteria from the development plan.
package framework_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	fwk "k8s.io/kube-scheduler/framework"
	schedulerconfig "k8s.io/kubernetes/pkg/scheduler/apis/config"
	"k8s.io/kubernetes/pkg/scheduler/backend/cache"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/feature"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/noderesources"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/queuesort"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/tainttoleration"
	frameworkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	"k8s.io/kubernetes/pkg/scheduler/metrics"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	testingframework "k8s.io/kubernetes/pkg/scheduler/testing/framework"
	"sigs.k8s.io/yaml"

	"github.com/SakkijarvenPolka/tahrs-scheduler/internal/config"
	"github.com/SakkijarvenPolka/tahrs-scheduler/internal/plugin/tahrs"
	profilepkg "github.com/SakkijarvenPolka/tahrs-scheduler/internal/profile"
)

const (
	schedulerName = "tahrs-scheduler"
	gpuResource   = "nvidia.com/gpu"
	// scoreWeight mirrors the sample KubeSchedulerConfiguration: the framework
	// multiplies the plugin's 0-100 score by the weight of its Score entry.
	scoreWeight int32 = 5
)

// TestMain installs the scheduler's own metrics because the framework runtime
// instruments every plugin through them (setInstrumentedPlugins).
func TestMain(m *testing.M) {
	metrics.Register()
	os.Exit(m.Run())
}

// env is a scheduler framework wired with the Tahrs plugin and, optionally, the
// native plugins a cluster profile runs alongside it.
type env struct {
	t         *testing.T
	framework framework.Framework
	nodes     []*v1.Node
	nodeInfos []fwk.NodeInfo
	config    *config.Loaded
}

// newEnv builds the framework for nodes. The plugin configuration is written to
// a temp file in the same shape kube-scheduler passes through --config, so the
// tests exercise the real YAML loader and hashing code.
func newEnv(t *testing.T, setting config.Setting, nodes []*v1.Node, withNativePlugins bool) *env {
	t.Helper()
	return newEnvWithResident(t, setting, nodes, nil, withNativePlugins)
}

// newEnvWithResident is newEnv plus pods already bound to nodes, which is how the
// framework learns how much of each node is consumed.
func newEnvWithResident(t *testing.T, setting config.Setting, nodes []*v1.Node, resident []*v1.Pod, withNativePlugins bool) *env {
	t.Helper()
	cfgPath, loaded := writeConfig(t, setting)
	return newEnvWithConfig(t, cfgPath, loaded, nodes, resident, withNativePlugins)
}

// newEnvWithConfig wires the framework around an already loaded configuration, so
// replay and framework assertions can share one config hash.
func newEnvWithConfig(t *testing.T, cfgPath string, loaded *config.Loaded, nodes []*v1.Node, resident []*v1.Pod, withNativePlugins bool) *env {
	t.Helper()

	fns := []testingframework.RegisterPluginFunc{tahrsRegistrar(cfgPath)}
	if withNativePlugins {
		fns = append(fns, nativePluginsRegistrar)
	}

	snapshot := cache.NewSnapshot(resident, nodes)
	f, err := testingframework.NewFramework(
		context.Background(),
		fns,
		schedulerName,
		frameworkruntime.WithSnapshotSharedLister(snapshot),
		// High enough that the 16-node concurrency test really runs in parallel.
		frameworkruntime.WithParallelism(16),
	)
	if err != nil {
		t.Fatalf("build framework: %v", err)
	}

	infos, err := snapshot.NodeInfos().List()
	if err != nil {
		t.Fatalf("list node infos: %v", err)
	}

	return &env{t: t, framework: f, nodes: nodes, nodeInfos: infos, config: loaded}
}

// writeConfig serialises setting to a temporary file and loads it back through
// the production loader, returning both the path and the loaded configuration.
func writeConfig(t *testing.T, setting config.Setting) (string, *config.Loaded) {
	t.Helper()

	data, err := yaml.Marshal(setting)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	path := filepath.Join(t.TempDir(), "tahrs-config.yaml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	loaded, err := config.Load(data)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return path, loaded
}

// tahrsRegistrar wires the plugin exactly as the sample KubeSchedulerConfiguration
// does: PreFilter, PreScore and a weighted Score plugin, with args carrying the
// config file path.
func tahrsRegistrar(cfgPath string) testingframework.RegisterPluginFunc {
	return func(reg *frameworkruntime.Registry, profile *schedulerconfig.KubeSchedulerProfile) {
		reg.Register(tahrs.Name, tahrs.New)
		// The runtime demands exactly one QueueSort plugin per profile. It plays
		// no part in these tests, which exercise single-pod cycles.
		reg.Register(queuesort.Name, queuesort.New)
		profile.Plugins.QueueSort.Enabled = append(profile.Plugins.QueueSort.Enabled,
			schedulerconfig.Plugin{Name: queuesort.Name})
		// The runtime also demands a Bind plugin; these tests evaluate Filter and
		// Score only, so a no-op binder is enough.
		reg.Register(noopBinderName, func(context.Context, runtime.Object, fwk.Handle) (fwk.Plugin, error) {
			return noopBinder{}, nil
		})
		profile.Plugins.Bind.Enabled = append(profile.Plugins.Bind.Enabled,
			schedulerconfig.Plugin{Name: noopBinderName})

		profile.Plugins.PreFilter.Enabled = append(profile.Plugins.PreFilter.Enabled,
			schedulerconfig.Plugin{Name: tahrs.Name})
		profile.Plugins.PreScore.Enabled = append(profile.Plugins.PreScore.Enabled,
			schedulerconfig.Plugin{Name: tahrs.Name})
		profile.Plugins.Score.Enabled = append(profile.Plugins.Score.Enabled,
			schedulerconfig.Plugin{Name: tahrs.Name, Weight: scoreWeight})

		profile.PluginConfig = append(profile.PluginConfig, schedulerconfig.PluginConfig{
			Name: tahrs.Name,
			Args: &tahrs.Args{ConfigPath: cfgPath},
		})
	}
}

// nativePluginsRegistrar adds TaintToleration (Filter) and NodeResourcesFit
// (PreFilter + Filter). NodeResourcesFit must run its PreFilter or its Filter
// cannot read the pod request state it depends on.
func nativePluginsRegistrar(reg *frameworkruntime.Registry, profile *schedulerconfig.KubeSchedulerProfile) {
	reg.Register(noderesources.Name, func(ctx context.Context, obj runtime.Object, h fwk.Handle) (fwk.Plugin, error) {
		args, ok := obj.(*schedulerconfig.NodeResourcesFitArgs)
		if !ok {
			return nil, fmt.Errorf("NodeResourcesFit: want args of type *NodeResourcesFitArgs, got %T", obj)
		}
		return noderesources.NewFit(ctx, args, h, feature.Features{})
	})
	reg.Register(tainttoleration.Name, func(ctx context.Context, obj runtime.Object, h fwk.Handle) (fwk.Plugin, error) {
		return tainttoleration.New(ctx, obj, h, feature.Features{})
	})

	profile.Plugins.PreFilter.Enabled = append(profile.Plugins.PreFilter.Enabled,
		schedulerconfig.Plugin{Name: noderesources.Name})
	profile.Plugins.Filter.Enabled = append(profile.Plugins.Filter.Enabled,
		schedulerconfig.Plugin{Name: noderesources.Name},
		schedulerconfig.Plugin{Name: tainttoleration.Name})
	profile.PluginConfig = append(profile.PluginConfig, schedulerconfig.PluginConfig{
		Name: noderesources.Name,
		Args: &schedulerconfig.NodeResourcesFitArgs{
			ScoringStrategy: &schedulerconfig.ScoringStrategy{Type: schedulerconfig.LeastAllocated},
		},
	})
}

// noopBinder satisfies the runtime's requirement that a profile has a Bind
// plugin. Nothing in these tests binds a pod.
type noopBinder struct{}

const noopBinderName = "TestBinder"

func (noopBinder) Name() string { return noopBinderName }

func (noopBinder) Bind(context.Context, fwk.CycleState, *v1.Pod, string) *fwk.Status { return nil }

// result captures one scheduling cycle: the PreFilter verdict, every node's
// Filter verdict, the nodes that survived, and the plugin's score per node.
type result struct {
	preFilter     *fwk.Status
	filterStatus  map[string]fwk.Code
	filterMessage map[string]string
	feasible      []string
	scoring       *fwk.Status
	scores        map[string]int64
}

// run executes the framework in the same order the scheduling cycle does:
// PreFilter, then Filter per node, then PreScore and Score over the surviving
// nodes only. When Filter leaves no node, scoring is skipped, as upstream does.
func (e *env) run(pod *v1.Pod) *result {
	e.t.Helper()

	res := &result{
		filterStatus:  make(map[string]fwk.Code, len(e.nodeInfos)),
		filterMessage: make(map[string]string, len(e.nodeInfos)),
		scores:        make(map[string]int64, len(e.nodeInfos)),
	}
	ctx := context.Background()

	state := framework.NewCycleState()
	if _, status, _ := e.framework.RunPreFilterPlugins(ctx, state, pod); statusCode(status) != fwk.Success {
		res.preFilter = status
		return res
	}

	feasible := make([]fwk.NodeInfo, 0, len(e.nodeInfos))
	for _, node := range e.nodeInfos {
		name := node.Node().Name
		status := e.framework.RunFilterPlugins(ctx, state, pod, node)
		res.filterStatus[name] = statusCode(status)
		if statusCode(status) == fwk.Success {
			feasible = append(feasible, node)
			res.feasible = append(res.feasible, name)
			continue
		}
		res.filterMessage[name] = status.Message()
	}
	if len(feasible) == 0 {
		return res
	}

	if status := e.framework.RunPreScorePlugins(ctx, state, pod, feasible); statusCode(status) != fwk.Success {
		res.scoring = status
		return res
	}
	scored, status := e.framework.RunScorePlugins(ctx, state, pod, feasible)
	if statusCode(status) != fwk.Success {
		res.scoring = status
		return res
	}
	for _, nodeScores := range scored {
		for _, pluginScore := range nodeScores.Scores {
			if pluginScore.Name == tahrs.Name {
				res.scores[nodeScores.Name] = pluginScore.Score
			}
		}
	}
	return res
}

// ranking orders nodes the way the scheduler would: highest score first, ties
// broken by name so every assertion is deterministic.
func (r *result) ranking() []string {
	names := make([]string, 0, len(r.scores))
	for name := range r.scores {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if r.scores[names[i]] != r.scores[names[j]] {
			return r.scores[names[i]] > r.scores[names[j]]
		}
		return names[i] < names[j]
	})
	return names
}

// statusCode treats a nil status as success, which is what the framework does.
func statusCode(s *fwk.Status) fwk.Code {
	if s == nil {
		return fwk.Success
	}
	return s.Code()
}

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

func cpuGeneralSetting() config.Setting {
	return config.Setting{
		ConfigVersion:   "test-2026-10",
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
		},
	}
}

func gpuInferenceSetting() config.Setting {
	setting := cpuGeneralSetting()
	setting.Profiles["gpu-inference-v1"] = config.Profile{
		Alpha:         0.3,
		Beta:          0.7,
		FitWeights:    map[string]float64{"cpu": 1, "memory": 1, "gpu": 3},
		PackWeights:   map[string]float64{"cpu": 1, "memory": 1, "gpu": 2},
		GPUNodePolicy: "allow",
	}
	return setting
}

// node builds a node with explicit allocatable resources. Pod capacity has to be
// set by hand: passing a Capacity map replaces the wrapper's default, and a zero
// pod budget would make NodeResourcesFit reject every pod.
func node(name, cpu, memory string, gpus int64, taints ...v1.Taint) *v1.Node {
	capacity := map[v1.ResourceName]string{
		v1.ResourceCPU:    cpu,
		v1.ResourceMemory: memory,
		v1.ResourcePods:   "32",
	}
	if gpus > 0 {
		capacity[v1.ResourceName(gpuResource)] = fmt.Sprintf("%d", gpus)
	}
	wrapper := st.MakeNode().Name(name).Capacity(capacity)
	if len(taints) > 0 {
		wrapper = wrapper.Taints(taints)
	}
	return wrapper.Obj()
}

// pod builds a pod requesting resources, optionally annotated with a profile and
// tolerating taints.
func pod(profile string, requests map[v1.ResourceName]string, tolerations ...v1.Toleration) *v1.Pod {
	wrapper := st.MakePod().
		Name("workload").
		Namespace("default").
		SchedulerName(schedulerName).
		Req(requests)
	if profile != "" {
		wrapper = wrapper.Annotation(profilepkg.AnnotationKey, profile)
	}
	if len(tolerations) > 0 {
		wrapper = wrapper.Tolerations(tolerations)
	}
	return wrapper.Obj()
}
