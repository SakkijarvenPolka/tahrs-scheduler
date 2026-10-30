package tahrs

import (
	"context"
	"errors"
	"sync"
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

// Reasons recorded in tahrs_gpu_ledger_blind_total{reason=...}. Both describe a
// cluster shape, never a Pod or node.
const (
	ledgerBlindExtendedResource = "extended_resource"
	ledgerBlindDeviceClaims     = "device_claims"
)

// warnGPULedgerBlindOnce keeps the DRA blindness warning to one line per
// process. It describes a configuration shape, not a per-Pod event, and would
// otherwise be logged for every Pod of every cycle.
var warnGPULedgerBlindOnce sync.Once

// PreScore implements fwk.PreScorePlugin. PreFilter already did the work; this
// only reports a missing CycleState, which would be an internal error, and
// records the one degradation Score cannot see on its own: a cycle in which the
// GPU dimension is blind.
func (p *Plugin) PreScore(_ context.Context, state fwk.CycleState, pod *v1.Pod, nodes []fwk.NodeInfo) *fwk.Status {
	data, err := readState(state)
	if err != nil {
		return fwk.AsStatus(err)
	}
	reason := gpuLedgerBlindReason(data.Requests, usesDeviceClaims(pod), nodes, p.gpuResourceName())
	if reason == "" {
		return nil
	}
	p.metrics.GPULedgerBlind(reason)
	warnGPULedgerBlindOnce.Do(func() {
		klog.InfoS("GPU dimension has no allocatable source, GPU Pods are scored MinScore",
			"gpuResource", p.gpuResourceName(),
			"reason", reason,
			"pod", klog.KObj(pod),
			"hint", "devices allocated through DRA are published as ResourceSlices and never raise the extended resource allocatable")
	})
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
	breakdown, err := scoring.Evaluate(data.Requests,
		kubeadapter.NodeSnapshot(nodeInfo, p.gpuResourceName()),
		data.Resolution.Profile,
		scoring.WithDeviceClaims(usesDeviceClaims(pod)))
	if err != nil {
		p.metrics.InvalidNumeric(failureKind(err))
		logger.Error(err, "Scored node with tahrs fallback",
			"pod", klog.KObj(pod), "node", nodeName, "profile", data.Resolution.Name)
		return fwk.MinScore, nil
	}

	p.metrics.ObserveScore(time.Since(start), breakdown)
	return breakdown.Final, nil
}

// usesDeviceClaims reports whether the Pod holds resources through DRA device
// claims (spec.resourceClaims, either an existing claim or a template). Such a
// Pod may consume a device without declaring an extended resource request.
func usesDeviceClaims(pod *v1.Pod) bool {
	return pod != nil && len(pod.Spec.ResourceClaims) > 0
}

// gpuLedgerBlindReason reports why the GPU dimension cannot see this Pod's
// device demand, or "" when it can.
//
// tahrs reads node allocatable and the Pod's request vector only; it never reads
// ResourceSlices. Two shapes therefore degrade silently and are counted:
//
//   - a Pod that asks for GPUs through an extended resource which the DRA bridge
//     backs. The scheduler lets it through filtering (the framework skips the
//     insufficient-resource check for DRA backed resources when allocatable is
//     zero), but allocatable stays zero, so Pack has no fill ratio and
//     Evaluate fails with ErrGPUNodeInsufficient.
//   - a Pod that only holds claims, which never enters the request vector at
//     all.
//
// An empty node list means Score will not run, so nothing is counted.
func gpuLedgerBlindReason(requests model.TaskRequests, hasDeviceClaims bool, nodes []fwk.NodeInfo, gpuResource v1.ResourceName) string {
	if len(nodes) == 0 {
		return ""
	}
	for _, nodeInfo := range nodes {
		if nodeInfo == nil {
			continue
		}
		if nodeInfo.GetAllocatable().GetScalarResources()[gpuResource] > 0 {
			return ""
		}
	}
	switch {
	case requests.GPUs > 0:
		return ledgerBlindExtendedResource
	case hasDeviceClaims:
		return ledgerBlindDeviceClaims
	default:
		return ""
	}
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
