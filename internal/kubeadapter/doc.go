// Package kubeadapter translates Kubernetes scheduling objects into the plain
// data model used by the scoring engine (internal/model).
//
// It is the only place inside the scheduler plugin that is allowed to import
// k8s.io/* types for the purpose of reading resource quantities: internal/model
// and internal/scoring must stay free of Kubernetes dependencies so that the
// arithmetic can be tested (and replayed offline) without a cluster.
//
// The translation deliberately mirrors what the in-tree scheduler does:
//
//   - CPU and memory demand is the Pod-level request aggregation implemented by
//     k8s.io/component-helpers/resource (max over init containers, cumulative
//     restartable init containers / native sidecars, Pod overhead).
//   - Extended resources such as GPUs are incompressible, so a container that
//     declares only a limit still consumes the full amount.
//   - Node usage is read from fwk.NodeInfo.GetRequested(), i.e. the scheduler's
//     own ledger, never from a live API call.
package kubeadapter
