package scoring

// Option adjusts how a score is computed for Pod properties that the resource
// vector cannot express.
//
// Options stay optional because the vector remains the only input on the plain
// path: golden files and the replay tool compare requests, node state and
// profile only, and must keep producing the same numbers.
type Option func(*evalConfig)

// evalConfig is the resolved option set. It is a value type so that Evaluate
// can hand it to GPUPenalty without allocating.
type evalConfig struct {
	deviceClaims bool
}

// WithDeviceClaims reports whether the Pod references DRA device claims
// (spec.resourceClaims). Such a Pod can hold a GPU without declaring an
// extended resource request, because DRA allocates whole devices instead of
// counting them. The "this Pod does not want a GPU" penalty must therefore not
// apply: a claim-backed Pod placed on a GPU node usually asked for exactly
// that.
//
// Pass the condition directly; a false value is a no-op:
//
//	scoring.Evaluate(req, snap, view, scoring.WithDeviceClaims(len(pod.Spec.ResourceClaims) > 0))
func WithDeviceClaims(hasDeviceClaims bool) Option {
	if !hasDeviceClaims {
		return func(*evalConfig) {}
	}
	return func(cfg *evalConfig) { cfg.deviceClaims = true }
}

// resolveOptions applies opts in order. A nil Option is ignored so callers can
// forward a conditionally built slice.
func resolveOptions(opts []Option) evalConfig {
	var cfg evalConfig
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	return cfg
}
