package config

import (
	"fmt"
	"sort"

	"github.com/SakkijarvenPolka/tahrs-scheduler/internal/model"
)

// Compile turns the validated configuration into the immutable views used by
// the scoring library. Dimension names are mapped to model.Weights here, so the
// plugin never parses strings on the hot path.
func (s *Setting) Compile() (map[string]model.ProfileView, error) {
	views := make(map[string]model.ProfileView, len(s.Profiles))
	for name, p := range s.Profiles {
		fit, err := compileWeights(p.FitWeights)
		if err != nil {
			return nil, fmt.Errorf("profile %q fitWeights: %w", name, err)
		}
		pack, err := compileWeights(p.PackWeights)
		if err != nil {
			return nil, fmt.Errorf("profile %q packWeights: %w", name, err)
		}
		view := model.ProfileView{
			Name:            name,
			Alpha:           p.Alpha,
			Beta:            p.Beta,
			FitWeights:      fit,
			PackWeights:     pack,
			GPUPolicy:       model.GPUPolicy(p.GPUNodePolicy),
			GPUAvoidPenalty: s.GPUAvoidPenalty,
		}
		if err := view.Validate(); err != nil {
			return nil, fmt.Errorf("profile %q: %w", name, err)
		}
		views[name] = view
	}
	return views, nil
}

// ProfileNames returns the compiled profile names in sorted order.
func ProfileNames(views map[string]model.ProfileView) []string {
	names := make([]string, 0, len(views))
	for name := range views {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func compileWeights(in map[string]float64) (model.Weights, error) {
	var w model.Weights
	for name, v := range in {
		switch name {
		case DimCPU:
			w.CPU = v
		case DimMemory:
			w.Memory = v
		case DimGPU:
			w.GPU = v
		default:
			return model.Weights{}, fmt.Errorf("unknown dimension %q", name)
		}
	}
	if err := w.Validate("compiled"); err != nil {
		return model.Weights{}, err
	}
	return w, nil
}
