package tahrs

import (
	"context"
	"strings"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	fwk "k8s.io/kube-scheduler/framework"

	"github.com/SakkijarvenPolka/tahrs-scheduler/internal/profile"
)

// profileAnnotation returns the profile requested by a Pod, trimmed exactly the
// way profile.Resolve trims it, so the enqueue hint and PreFilter agree on what
// "the annotation changed" means.
func profileAnnotation(pod *v1.Pod) string {
	if pod == nil {
		return ""
	}
	return strings.TrimSpace(pod.Annotations[profile.AnnotationKey])
}

// EventsToRegister narrows requeueing to the only event that can resolve a
// PreFilter rejection: an update of the unscheduled Pod itself.
//
// Without this method the scheduler installs a wildcard extension for every
// PreFilter plugin (framework/runtime/framework.go fillEnqueueExtensions), so a
// Pod rejected for an unknown profile would be retried on unrelated Node, PVC
// and other-Pod events. Those events can never fix an annotation, so the retries
// are pure overhead. Registering the generic Pod/Update action is required
// because an annotation edit is not one of the specific Pod update action types
// (there is no UpdatePodAnnotation); the hint below filters the rest.
func (p *Plugin) EventsToRegister(context.Context) ([]fwk.ClusterEventWithHint, error) {
	return []fwk.ClusterEventWithHint{
		{
			Event:          fwk.ClusterEvent{Resource: fwk.Pod, ActionType: fwk.Update},
			QueueingHintFn: p.profileAnnotationHint,
		},
	}, nil
}

// profileAnnotationHint queues a rejected Pod only when the update changes its
// profile annotation into something the running configuration can resolve:
// another known profile, or no annotation at all (which falls back to the
// default profile). Removing an unknown annotation therefore re-activates the
// Pod, while an unrelated update leaves it parked.
func (p *Plugin) profileAnnotationHint(_ klog.Logger, _ *v1.Pod, oldObj, newObj interface{}) (fwk.QueueingHint, error) {
	oldPod, _ := oldObj.(*v1.Pod)
	newPod, ok := newObj.(*v1.Pod)
	if !ok || newPod == nil {
		return fwk.QueueSkip, nil
	}

	before := profileAnnotation(oldPod)
	after := profileAnnotation(newPod)
	if before == after {
		return fwk.QueueSkip, nil
	}
	if after == "" {
		// Default profile; startup validation guarantees it exists.
		return fwk.Queue, nil
	}
	if _, known := p.views[after]; known {
		return fwk.Queue, nil
	}
	return fwk.QueueSkip, nil
}
