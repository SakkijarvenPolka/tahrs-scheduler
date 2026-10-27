package tahrs

import (
	"context"
	"errors"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	fwk "k8s.io/kube-scheduler/framework"

	"github.com/SakkijarvenPolka/tahrs-scheduler/internal/kubeadapter"
	"github.com/SakkijarvenPolka/tahrs-scheduler/internal/model"
	"github.com/SakkijarvenPolka/tahrs-scheduler/internal/scoring"
)

// Failure kinds recorded in tahrs_invalid_numeric_total{kind=...}. The set is
// static on purpose: metric labels must not grow with Pod or node names.
const (
	kindGPUNodeInsufficient = "gpu_node_insufficient"
	kindInvalidNumeric      = "invalid_numeric"
	kindInvalidProfile      = "invalid_profile"
	kindMissingState        = "missing_state"
)

// PreScore implements fwk.PreScorePlugin. PreFilter already did the work; this
// only reports a missing CycleState, which would be an internal error.
func (p *Plugin) PreScore(_ context.Context, state fwk.CycleState, _ *v1.Pod, _ []fwk.NodeInfo) *fwk.Status {
	if _, err := readState(state); err != nil {
		return fwk.AsStatus(err)
	}
	return nil
}

// Score implements fwk.ScorePlugin (plan §6.4).
//
// The returned status is always nil: the scheduling framework aborts the whole
// Pod when a Score plugin returns a non-success status (R7), so an unevaluable
// node is scored MinScore and counted instead of failing the Pod.
func (p *Plugin) Score(ctx context.Context, state fwk.CycleState, pod *v1.Pod, nodeInfo fwk.NodeInfo) (int64, *fwk.Status) {
	start := time.Now()
	logger := klog.FromContext(ctx)

	data, err := readState(state)
	if err != nil {
		p.metrics.InvalidNumeric(kindMissingState)
		logger.Error(err, "Skipped tahrs scoring, cycle state missing", "pod", klog.KObj(pod))
		return fwk.MinScore, nil
	}

	nodeName := ""
	if node := nodeInfo.Node(); node != nil {
		nodeName = node.Name
	}
	breakdown, err := scoring.Evaluate(data.Requests, kubeadapter.NodeSnapshot(nodeInfo, p.gpuResourceName()), data.Resolution.Profile)
	if err != nil {
		p.metrics.InvalidNumeric(failureKind(err))
		logger.Error(err, "Scored node with tahrs fallback",
			"pod", klog.KObj(pod), "node", nodeName, "profile", data.Resolution.Name)
		return fwk.MinScore, nil
	}

	p.metrics.ObserveScore(time.Since(start), breakdown)
	return breakdown.Final, nil
}

// ScoreExtensions implements fwk.ScorePlugin. tahrs returns itself so that the
// NormalizeScore passthrough is explicit.
func (p *Plugin) ScoreExtensions() fwk.ScoreExtensions { return p }

// NormalizeScore implements fwk.ScoreExtensions and passes scores through
// unchanged: the score is already defined on [MinScore, MaxScore].
func (p *Plugin) NormalizeScore(_ context.Context, _ fwk.CycleState, _ *v1.Pod, _ fwk.NodeScoreList) *fwk.Status {
	return nil
}

func failureKind(err error) string {
	switch {
	case errors.Is(err, model.ErrGPUNodeInsufficient):
		return kindGPUNodeInsufficient
	case errors.Is(err, model.ErrInvalidNumeric):
		return kindInvalidNumeric
	default:
		return kindInvalidProfile
	}
}
