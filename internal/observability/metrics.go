// Package observability holds the tahrs-scheduler metrics (plan §8).
//
// Metrics never influence a placement decision: the scoring library stays pure
// and the plugin only records what already happened.
package observability

import (
	"sync"
	"time"

	"k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"

	"github.com/SakkijarvenPolka/tahrs-scheduler/internal/model"
	"github.com/SakkijarvenPolka/tahrs-scheduler/internal/profile"
)

// Namespace prefixes every metric so they cannot collide with the kube
// scheduler's own series.
const Namespace = "tahrs"

// Metrics bundles every series the plugin writes.
type Metrics struct {
	profileResolution *metrics.CounterVec
	scoreDuration     *metrics.HistogramVec
	scoreComponent    *metrics.HistogramVec
	invalidNumeric    *metrics.CounterVec
	gpuPenalty        *metrics.CounterVec
	gpuLedgerBlind    *metrics.CounterVec
	configInfo        *metrics.GaugeVec
}

var (
	defaultOnce    sync.Once
	defaultMetrics *Metrics
)

// Default returns the process wide metrics set, registering it with the legacy
// registry on first use. Registration happens once per process because
// duplicate registration panics.
func Default() *Metrics {
	defaultOnce.Do(func() {
		defaultMetrics = New()
		defaultMetrics.register()
	})
	return defaultMetrics
}

// New builds the metric set without registering it. Tests use it directly.
func New() *Metrics {
	return &Metrics{
		profileResolution: metrics.NewCounterVec(&metrics.CounterOpts{
			Namespace: Namespace,
			Name:      "profile_resolution_total",
			Help:      "Number of profile resolutions, by result.",
		}, []string{"result"}),
		scoreDuration: metrics.NewHistogramVec(&metrics.HistogramOpts{
			Namespace: Namespace,
			Name:      "score_duration_seconds",
			Help:      "Wall clock duration of one tahrs Score call.",
			Buckets:   metrics.ExponentialBuckets(0.000005, 2, 12),
		}, nil),
		scoreComponent: metrics.NewHistogramVec(&metrics.HistogramOpts{
			Namespace: Namespace,
			Name:      "score_component",
			Help:      "Distribution of the Fit and Pack components of the tahrs score.",
			Buckets:   metrics.LinearBuckets(0, 0.1, 11),
		}, []string{"component", "profile"}),
		invalidNumeric: metrics.NewCounterVec(&metrics.CounterOpts{
			Namespace: Namespace,
			Name:      "invalid_numeric_total",
			Help:      "Number of nodes scored with a fallback because evaluation failed.",
		}, []string{"kind"}),
		gpuPenalty: metrics.NewCounterVec(&metrics.CounterOpts{
			Namespace: Namespace,
			Name:      "gpu_penalty_total",
			Help:      "Number of times the GPU avoid policy was applied.",
		}, []string{"profile"}),
		gpuLedgerBlind: metrics.NewCounterVec(&metrics.CounterOpts{
			Namespace: Namespace,
			Name:      "gpu_ledger_blind_total",
			Help:      "Number of scheduling cycles in which the GPU dimension could not see the Pod's device demand, by reason.",
		}, []string{"reason"}),
		configInfo: metrics.NewGaugeVec(&metrics.GaugeOpts{
			Namespace: Namespace,
			Name:      "config_info",
			Help:      "Loaded configuration, as a constant 1 labelled with version and hash.",
		}, []string{"configVersion", "configHash"}),
	}
}

func (m *Metrics) register() {
	legacyregistry.MustRegister(
		m.profileResolution,
		m.scoreDuration,
		m.scoreComponent,
		m.invalidNumeric,
		m.gpuPenalty,
		m.gpuLedgerBlind,
		m.configInfo,
	)
}

// ProfileResolution records how a Pod's profile was chosen.
func (m *Metrics) ProfileResolution(source profile.Source) {
	if m == nil {
		return
	}
	m.profileResolution.WithLabelValues(string(source)).Inc()
}

// ObserveScore records duration and components of a successful evaluation.
func (m *Metrics) ObserveScore(d time.Duration, bd model.Breakdown) {
	if m == nil {
		return
	}
	m.scoreDuration.WithLabelValues().Observe(d.Seconds())
	m.scoreComponent.WithLabelValues("fit", bd.Profile).Observe(bd.Fit)
	m.scoreComponent.WithLabelValues("pack", bd.Profile).Observe(bd.Pack)
	if bd.GPUPenalty > 0 {
		m.gpuPenalty.WithLabelValues(bd.Profile).Inc()
	}
}

// InvalidNumeric records a node that fell back to MinScore. kind groups the
// failure reason ("gpu_node_insufficient", "state", ...) and must stay a small,
// bounded set (no Pod or node names as labels).
func (m *Metrics) InvalidNumeric(kind string) {
	if m == nil {
		return
	}
	m.invalidNumeric.WithLabelValues(kind).Inc()
}

// GPULedgerBlind records a cycle in which the GPU dimension had no allocatable
// source: no feasible node advertised the configured extended resource. reason
// separates the two shapes this takes ("extended_resource" for a Pod that asks
// for GPUs through a DRA bridged extended resource, "device_claims" for a Pod
// that only holds claims). Both are bounded strings, never Pod or node names.
func (m *Metrics) GPULedgerBlind(reason string) {
	if m == nil {
		return
	}
	m.gpuLedgerBlind.WithLabelValues(reason).Inc()
}

// RecordConfig exposes the loaded configuration as an info gauge.
func (m *Metrics) RecordConfig(configVersion, configHash string) {
	if m == nil {
		return
	}
	m.configInfo.WithLabelValues(configVersion, configHash).Set(1)
}
