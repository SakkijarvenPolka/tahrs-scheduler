// Package profile maps a Pod to the scoring profile it asks for.
//
// Resolution happens in PreFilter, never in Score: when only one node survives
// filtering the scheduler skips PreScore and Score entirely (F13), so an
// unknown profile would otherwise be scheduled with arbitrary scores.
package profile

import (
	"fmt"
	"sort"
	"strings"

	"github.com/SakkijarvenPolka/tahrs-scheduler/internal/model"
)

// AnnotationKey is the Pod annotation selecting a profile (plan §9).
const AnnotationKey = "scheduling.example.com/profile"

// Source records why a profile was chosen; it is exported as a metric label.
type Source string

const (
	// SourceAnnotation means the Pod annotation selected an existing profile.
	SourceAnnotation Source = "resolved"
	// SourceDefault means the Pod had no annotation and the default applied.
	SourceDefault Source = "default"
	// SourceUnknown means the Pod annotation named a profile that is not
	// configured; the Pod is rejected in PreFilter.
	SourceUnknown Source = "unknown"
)

// UnknownProfileError is returned when the Pod annotation names a profile the
// running configuration does not define.
type UnknownProfileError struct {
	Requested string
	Available []string
}

func (e *UnknownProfileError) Error() string {
	return fmt.Sprintf("unknown profile %q (available: %s)", e.Requested, strings.Join(e.Available, ", "))
}

// Resolution is the immutable result stored in CycleState by PreFilter.
type Resolution struct {
	Name    string
	Source  Source
	Profile model.ProfileView
}

// Resolve picks the profile for a Pod.
//
// A missing or empty annotation selects defaultProfile. An annotation naming an
// unknown profile returns *UnknownProfileError so the caller can reject the Pod
// with UnschedulableAndUnresolvable; a missing default profile is a
// configuration bug and returns a plain error.
func Resolve(annotations map[string]string, defaultProfile string, views map[string]model.ProfileView) (Resolution, error) {
	requested := strings.TrimSpace(annotations[AnnotationKey])
	if requested == "" {
		view, ok := views[defaultProfile]
		if !ok {
			return Resolution{Source: SourceUnknown}, fmt.Errorf("default profile %q is not compiled", defaultProfile)
		}
		return Resolution{Name: defaultProfile, Source: SourceDefault, Profile: view}, nil
	}
	view, ok := views[requested]
	if !ok {
		return Resolution{Name: requested, Source: SourceUnknown}, &UnknownProfileError{
			Requested: requested,
			Available: sortedNames(views),
		}
	}
	return Resolution{Name: requested, Source: SourceAnnotation, Profile: view}, nil
}

func sortedNames(views map[string]model.ProfileView) []string {
	names := make([]string, 0, len(views))
	for name := range views {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
