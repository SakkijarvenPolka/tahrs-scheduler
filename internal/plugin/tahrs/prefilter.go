package tahrs

import (
	"context"
	"errors"
	"fmt"
	"strings"

	v1 "k8s.io/api/core/v1"
	fwk "k8s.io/kube-scheduler/framework"

	"github.com/SakkijarvenPolka/tahrs-scheduler/internal/kubeadapter"
	"github.com/SakkijarvenPolka/tahrs-scheduler/internal/profile"
)

// PreFilter resolves the profile, computes the Pod's requests and stores both
// in CycleState (plan §6.3).
//
// Everything that must hold for every Pod lives here, because the scheduler
// skips PreScore and Score when a single node survives Filter (F13). An unknown
// profile is therefore rejected here with UnschedulableAndUnresolvable, and the
// reason carries the configuration hash so a rejected Pod can be traced back to
// the configuration that rejected it.
func (p *Plugin) PreFilter(_ context.Context, state fwk.CycleState, pod *v1.Pod, _ []fwk.NodeInfo) (*fwk.PreFilterResult, *fwk.Status) {
	resolution, err := profile.Resolve(pod.Annotations, p.setting.DefaultProfile, p.views)
	if err != nil {
		var unknown *profile.UnknownProfileError
		if errors.As(err, &unknown) {
			p.metrics.ProfileResolution(profile.SourceUnknown)
			reason := fmt.Sprintf("tahrs: unknown profile %q, available: %s (configHash %s)",
				unknown.Requested, strings.Join(unknown.Available, ", "), p.hash)
			return nil, fwk.NewStatus(fwk.UnschedulableAndUnresolvable, reason)
		}
		// A missing default profile is a configuration bug, not a Pod problem.
		return nil, fwk.AsStatus(fmt.Errorf("tahrs: %w", err))
	}
	p.metrics.ProfileResolution(resolution.Source)

	state.Write(stateKey, &stateData{
		Resolution: resolution,
		Requests:   kubeadapter.PodRequests(pod, p.gpuResourceName()),
	})
	return nil, nil
}

// PreFilterExtensions implements fwk.PreFilterPlugin. tahrs keeps no per-node
// state that would need incremental updates.
func (p *Plugin) PreFilterExtensions() fwk.PreFilterExtensions { return nil }
