// Package tahrs implements the tahrs-scheduler plugin: a PreFilter/PreScore/
// Score plugin for heterogeneous node pools (plan §6).
//
// Design constraints enforced here:
//   - Profile validation happens in PreFilter, because the scheduler skips
//     PreScore and Score when only one node survives filtering (F13).
//   - Score never returns an Error status; a node that cannot be evaluated gets
//     MinScore and is counted, so one bad node cannot fail the whole Pod (R7).
//   - The hot path performs no I/O: the configuration is read once in New.
package tahrs

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	v1 "k8s.io/api/core/v1"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	fwk "k8s.io/kube-scheduler/framework"

	"github.com/SakkijarvenPolka/tahrs-scheduler/internal/config"
	"github.com/SakkijarvenPolka/tahrs-scheduler/internal/model"
	"github.com/SakkijarvenPolka/tahrs-scheduler/internal/observability"
	"github.com/SakkijarvenPolka/tahrs-scheduler/internal/version"
)

// Name is the plugin name used in KubeSchedulerConfiguration.
const Name = "tahrs"

// ConfigPathEnv lets the plugin locate its configuration when the scheduler
// configuration does not carry args.configPath (useful for replay tooling and
// for running the binary with a hand written profile).
const ConfigPathEnv = "TAHRS_CONFIG_PATH"

// Args is the plugin argument object. Plan §6.5 keeps it to a single field.
type Args struct {
	apiruntime.TypeMeta `json:",inline"`
	ConfigPath          string `json:"configPath"`
}

// DeepCopyObject implements apiruntime.Object. The scheduler hands plugin args
// to the factory as a runtime.Object, so the typed args must satisfy it, which
// also lets tests put *Args directly into a KubeSchedulerProfile.
func (a *Args) DeepCopyObject() apiruntime.Object { return a.DeepCopy() }

// DeepCopy returns a copy of the args.
func (a *Args) DeepCopy() *Args {
	if a == nil {
		return nil
	}
	copied := *a
	return &copied
}

// Plugin implements fwk.PreFilterPlugin, fwk.PreScorePlugin and fwk.ScorePlugin.
type Plugin struct {
	handle  fwk.Handle
	setting *config.Setting
	views   map[string]model.ProfileView
	hash    string
	metrics *observability.Metrics
}

var (
	_ fwk.PreFilterPlugin = &Plugin{}
	_ fwk.PreScorePlugin  = &Plugin{}
	_ fwk.ScorePlugin     = &Plugin{}
	_ fwk.ScoreExtensions = &Plugin{}
)

// New is the PluginFactory registered by cmd/tahrs-scheduler. It reads and
// validates the configuration file once, logs the configuration fingerprint and
// returns the plugin.
func New(_ context.Context, args apiruntime.Object, h fwk.Handle) (fwk.Plugin, error) {
	decoded, err := decodeArgs(args)
	if err != nil {
		return nil, err
	}
	path := decoded.ConfigPath
	if path == "" {
		path = os.Getenv(ConfigPathEnv)
	}
	if path == "" {
		return nil, fmt.Errorf("tahrs: no configuration path: set pluginConfig args.configPath or %s", ConfigPathEnv)
	}

	loaded, err := config.LoadFile(path)
	if err != nil {
		return nil, fmt.Errorf("tahrs: %w", err)
	}
	views, err := loaded.Setting.Compile()
	if err != nil {
		return nil, fmt.Errorf("tahrs: %w", err)
	}

	metrics := observability.Default()
	metrics.RecordConfig(loaded.Setting.ConfigVersion, loaded.Hash)
	klog.InfoS("Loaded tahrs scheduler configuration", "config", loaded.Fingerprint(version.Get()))

	return &Plugin{
		handle:  h,
		setting: loaded.Setting,
		views:   views,
		hash:    loaded.Hash,
		metrics: metrics,
	}, nil
}

// Name implements fwk.Plugin.
func (p *Plugin) Name() string { return Name }

// decodeArgs accepts the two shapes the scheduler hands to a plugin factory:
// the raw Kubernetes object the framework decodes from pluginConfig args, and
// the typed Args when the profile is built programmatically (tests).
func decodeArgs(obj apiruntime.Object) (*Args, error) {
	if obj == nil {
		return &Args{}, nil
	}
	switch v := obj.(type) {
	case *Args:
		if v == nil {
			return &Args{}, nil
		}
		return v, nil
	case *apiruntime.Unknown:
		if v == nil || len(v.Raw) == 0 {
			return &Args{}, nil
		}
		args := &Args{}
		if err := json.Unmarshal(v.Raw, args); err != nil {
			return nil, fmt.Errorf("tahrs: decode plugin args: %w", err)
		}
		return args, nil
	default:
		return nil, fmt.Errorf("tahrs: unsupported plugin args type %T (want *Args or *runtime.Unknown)", obj)
	}
}

func (p *Plugin) gpuResourceName() v1.ResourceName { return v1.ResourceName(p.setting.GPUResourceName) }
