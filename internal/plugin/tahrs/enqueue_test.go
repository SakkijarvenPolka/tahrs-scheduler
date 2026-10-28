package tahrs

import (
	"context"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	fwk "k8s.io/kube-scheduler/framework"

	"github.com/SakkijarvenPolka/tahrs-scheduler/internal/model"
	"github.com/SakkijarvenPolka/tahrs-scheduler/internal/profile"
)

// runningConfig is a plugin whose views resolve the two sample profiles, as the
// real New builds from the configuration file.
func runningConfig() *Plugin {
	return &Plugin{views: map[string]model.ProfileView{
		"cpu-general-v1":   {},
		"gpu-inference-v1": {},
	}}
}

// annotated builds a pod carrying the profile annotation. hasAnnotation=false
// models an edit that removes the annotation entirely.
func annotated(profileValue string, hasAnnotation bool) *v1.Pod {
	p := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	if hasAnnotation {
		p.Annotations = map[string]string{profile.AnnotationKey: profileValue}
	}
	return p
}

// TestEventsToRegisterIsScopedToPodUpdates guards the reason the plugin
// implements EnqueueExtensions at all: without it the framework rewires the pod
// to every cluster event asynchronously, and a pod rejected for an unknown
// profile would be retried on unrelated Node/PVC events forever.
func TestEventsToRegisterIsScopedToPodUpdates(t *testing.T) {
	events, err := runningConfig().EventsToRegister(context.Background())
	if err != nil {
		t.Fatalf("EventsToRegister: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("registered %d events, want exactly 1 (the pod update): %+v", len(events), events)
	}
	event := events[0]
	if event.Event.Resource != fwk.Pod {
		t.Errorf("resource = %v, want %v", event.Event.Resource, fwk.Pod)
	}
	if event.Event.ActionType != fwk.Update {
		t.Errorf("action = %v, want %v (annotation edits surface as a generic pod update)", event.Event.ActionType, fwk.Update)
	}
	if event.QueueingHintFn == nil {
		t.Fatal("no queueing hint: the wildcard Pod/Update event would requeue every rejected pod")
	}
}

// TestProfileAnnotationHint checks when a parked pod becomes schedulable again.
func TestProfileAnnotationHint(t *testing.T) {
	cases := []struct {
		name string
		old  *v1.Pod
		new  *v1.Pod
		want fwk.QueueingHint
	}{
		{
			name: "same annotation",
			old:  annotated("no-such-profile", true),
			new:  annotated("no-such-profile", true),
			want: fwk.QueueSkip,
		},
		{
			name: "corrected to a known profile",
			old:  annotated("no-such-profile", true),
			new:  annotated("gpu-inference-v1", true),
			want: fwk.Queue,
		},
		{
			name: "annotation removed falls back to the default profile",
			old:  annotated("no-such-profile", true),
			new:  annotated("", false),
			want: fwk.Queue,
		},
		{
			name: "changed to another unknown profile",
			old:  annotated("no-such-profile", true),
			new:  annotated("also-wrong", true),
			want: fwk.QueueSkip,
		},
		{
			name: "whitespace only annotation falls back to the default profile",
			old:  annotated("no-such-profile", true),
			new:  annotated("   ", true),
			want: fwk.Queue,
		},
		{
			name: "non-pod new object",
			old:  annotated("no-such-profile", true),
			new:  nil,
			want: fwk.QueueSkip,
		},
	}

	p := runningConfig()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := p.profileAnnotationHint(klog.Background(), tc.old, tc.old, tc.new)
			if err != nil {
				t.Fatalf("hint: %v", err)
			}
			if got != tc.want {
				t.Fatalf("hint = %v, want %v", got, tc.want)
			}
		})
	}
}
