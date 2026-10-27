package profile

import (
	"errors"
	"strings"
	"testing"

	"github.com/SakkijarvenPolka/tahrs-scheduler/internal/model"
)

func views() map[string]model.ProfileView {
	return map[string]model.ProfileView{
		"cpu-general-v1":   {Name: "cpu-general-v1", Alpha: 0.3, Beta: 0.7},
		"gpu-inference-v1": {Name: "gpu-inference-v1", Alpha: 0.3, Beta: 0.7},
	}
}

func TestResolveAnnotation(t *testing.T) {
	res, err := Resolve(map[string]string{AnnotationKey: "gpu-inference-v1"}, "cpu-general-v1", views())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Name != "gpu-inference-v1" || res.Source != SourceAnnotation {
		t.Fatalf("resolution = %+v, want gpu-inference-v1/resolved", res)
	}
	if res.Profile.Name != "gpu-inference-v1" {
		t.Errorf("compiled view = %q", res.Profile.Name)
	}
}

func TestResolveEmptyAnnotationUsesDefault(t *testing.T) {
	for name, annotations := range map[string]map[string]string{
		"nil_map":    nil,
		"no_key":     {"other": "x"},
		"empty":      {AnnotationKey: ""},
		"whitespace": {AnnotationKey: "   "},
	} {
		t.Run(name, func(t *testing.T) {
			res, err := Resolve(annotations, "cpu-general-v1", views())
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if res.Name != "cpu-general-v1" || res.Source != SourceDefault {
				t.Fatalf("resolution = %+v, want the default profile", res)
			}
		})
	}
}

func TestResolveTrimsAnnotation(t *testing.T) {
	res, err := Resolve(map[string]string{AnnotationKey: "  gpu-inference-v1\n"}, "cpu-general-v1", views())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Name != "gpu-inference-v1" {
		t.Errorf("name = %q, want the trimmed annotation value", res.Name)
	}
}

func TestResolveUnknownProfile(t *testing.T) {
	_, err := Resolve(map[string]string{AnnotationKey: "no-such-profile"}, "cpu-general-v1", views())
	if err == nil {
		t.Fatal("Resolve accepted an unknown profile")
	}
	var unknown *UnknownProfileError
	if !errors.As(err, &unknown) {
		t.Fatalf("error = %v, want *UnknownProfileError", err)
	}
	if unknown.Requested != "no-such-profile" {
		t.Errorf("requested = %q", unknown.Requested)
	}
	want := []string{"cpu-general-v1", "gpu-inference-v1"}
	if len(unknown.Available) != len(want) {
		t.Fatalf("available = %v, want %v", unknown.Available, want)
	}
	for i := range want {
		if unknown.Available[i] != want[i] {
			t.Fatalf("available = %v, want sorted %v", unknown.Available, want)
		}
	}
	if !strings.Contains(err.Error(), "cpu-general-v1") {
		t.Errorf("error %q does not list the available profiles", err)
	}
}

// TestResolveCaseSensitive documents that profile names are matched exactly:
// "GPU-Inference-V1" is a different, unknown profile.
func TestResolveCaseSensitive(t *testing.T) {
	if _, err := Resolve(map[string]string{AnnotationKey: "GPU-Inference-V1"}, "cpu-general-v1", views()); err == nil {
		t.Fatal("Resolve matched a profile name case-insensitively")
	}
}

func TestResolveMissingDefaultIsAPlainError(t *testing.T) {
	_, err := Resolve(nil, "not-compiled", views())
	if err == nil {
		t.Fatal("Resolve accepted a missing default profile")
	}
	var unknown *UnknownProfileError
	if errors.As(err, &unknown) {
		t.Fatal("a missing default profile must not be reported as a user annotation error")
	}
	if !strings.Contains(err.Error(), "not-compiled") {
		t.Errorf("error %q does not name the default profile", err)
	}
}

func TestResolveEmptyViews(t *testing.T) {
	_, err := Resolve(map[string]string{AnnotationKey: "p"}, "", map[string]model.ProfileView{})
	var unknown *UnknownProfileError
	if !errors.As(err, &unknown) {
		t.Fatalf("error = %v, want *UnknownProfileError", err)
	}
	if len(unknown.Available) != 0 {
		t.Errorf("available = %v, want none", unknown.Available)
	}
}

func TestSourceStringsAreMetricSafe(t *testing.T) {
	for _, s := range []Source{SourceAnnotation, SourceDefault, SourceUnknown} {
		if s == "" || strings.ContainsAny(string(s), " -/\t\n") {
			t.Errorf("source %q is not a safe metric label value", s)
		}
	}
}
