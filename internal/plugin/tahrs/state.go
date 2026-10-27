package tahrs

import (
	"fmt"

	fwk "k8s.io/kube-scheduler/framework"

	"github.com/SakkijarvenPolka/tahrs-scheduler/internal/model"
	"github.com/SakkijarvenPolka/tahrs-scheduler/internal/profile"
)

// stateKey is the CycleState slot written by PreFilter and read by PreScore and
// Score.
const stateKey fwk.StateKey = "tahrs"

// stateData is immutable once written: Score is called concurrently for every
// candidate node and must not observe mutations.
type stateData struct {
	Resolution profile.Resolution
	Requests   model.TaskRequests
}

// Clone implements fwk.StateData. Sharing the pointer is safe because nothing
// mutates stateData after PreFilter.
func (s *stateData) Clone() fwk.StateData { return s }

func readState(state fwk.CycleState) (*stateData, error) {
	raw, err := state.Read(stateKey)
	if err != nil {
		return nil, fmt.Errorf("tahrs: cycle state is missing, PreFilter did not run: %w", err)
	}
	data, ok := raw.(*stateData)
	if !ok || data == nil {
		return nil, fmt.Errorf("tahrs: unexpected cycle state type %T", raw)
	}
	return data, nil
}
