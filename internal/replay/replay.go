// Package replay scores a Pod against a set of node snapshots without a
// cluster. It shares internal/scoring and internal/kubeadapter with the plugin,
// so the numbers printed by `tahrsctl replay` are the numbers the scheduler
// would return for the same input.
package replay

import (
	"fmt"
	"io"
	"sort"
	"text/tabwriter"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/SakkijarvenPolka/tahrs-scheduler/internal/config"
	"github.com/SakkijarvenPolka/tahrs-scheduler/internal/kubeadapter"
	"github.com/SakkijarvenPolka/tahrs-scheduler/internal/model"
	"github.com/SakkijarvenPolka/tahrs-scheduler/internal/profile"
	"github.com/SakkijarvenPolka/tahrs-scheduler/internal/scoring"
)

// Cluster is the set of nodes to score against.
type Cluster struct {
	Nodes []Node `json:"nodes" yaml:"nodes"`
}

// Node is one node snapshot. Allocatable and Requested use Kubernetes quantity
// strings keyed by "cpu", "memory" or the configured GPU resource name.
type Node struct {
	Name        string            `json:"name" yaml:"name"`
	Allocatable map[string]string `json:"allocatable" yaml:"allocatable"`
	Requested   map[string]string `json:"requested" yaml:"requested"`
}

// Workload is the Pod to place.
type Workload struct {
	Name     string            `json:"name" yaml:"name"`
	Profile  string            `json:"profile" yaml:"profile"`
	Requests map[string]string `json:"requests" yaml:"requests"`
}

// Row is one line of the replay table: everything the breakdown exposes.
type Row struct {
	Node        string  `json:"node"`
	Profile     string  `json:"profile"`
	Fit         float64 `json:"fit"`
	FitSkipped  bool    `json:"fitSkipped"`
	Pack        float64 `json:"pack"`
	PackSkipped bool    `json:"packSkipped"`
	Penalty     int64   `json:"gpuPolicyPenalty"`
	Score       int64   `json:"tahrsScore"`
	// Err records why a node could not be evaluated. Such a node is listed with
	// MinScore, exactly as the plugin scores it at runtime.
	Err string `json:"error,omitempty"`
}

// Result is the full replay output.
type Result struct {
	ConfigHash string `json:"configHash"`
	Profile    string `json:"profile"`
	Rows       []Row  `json:"rows"`
}

// Table scores every node in the cluster with the Pod's resolved profile.
//
// Rows are sorted by score descending, then by node name, which is the order the
// scheduler's tie breaking produces for equal scores.
func Table(loaded *config.Loaded, workload Workload, cluster Cluster) (*Result, error) {
	views, err := loaded.Setting.Compile()
	if err != nil {
		return nil, err
	}
	resolution, err := profile.Resolve(annotationFor(workload.Profile), loaded.Setting.DefaultProfile, views)
	if err != nil {
		return nil, err
	}
	requests, err := Requests(workload.Requests, loaded.Setting.GPUResourceName)
	if err != nil {
		return nil, err
	}

	rows := make([]Row, 0, len(cluster.Nodes))
	for _, node := range cluster.Nodes {
		allocatable, err := Resources(node.Allocatable, loaded.Setting.GPUResourceName)
		if err != nil {
			return nil, fmt.Errorf("node %q allocatable: %w", node.Name, err)
		}
		requested, err := Resources(node.Requested, loaded.Setting.GPUResourceName)
		if err != nil {
			return nil, fmt.Errorf("node %q requested: %w", node.Name, err)
		}
		snap := model.NodeSnapshot{Name: node.Name, Allocatable: allocatable, Used: requested}
		breakdown, err := scoring.Evaluate(requests, snap, resolution.Profile)
		if err != nil {
			// Replay is a diagnostic tool: list the broken node instead of
			// aborting the table. The plugin scores such a node MinScore too.
			rows = append(rows, Row{
				Node:    node.Name,
				Profile: resolution.Name,
				Score:   scoring.MinScore,
				Err:     err.Error(),
			})
			continue
		}
		rows = append(rows, Row{
			Node:        node.Name,
			Profile:     breakdown.Profile,
			Fit:         breakdown.Fit,
			FitSkipped:  breakdown.FitSkipped,
			Pack:        breakdown.Pack,
			PackSkipped: breakdown.PackSkipped,
			Penalty:     breakdown.GPUPenalty,
			Score:       breakdown.Final,
		})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Score != rows[j].Score {
			return rows[i].Score > rows[j].Score
		}
		return rows[i].Node < rows[j].Node
	})
	return &Result{ConfigHash: loaded.Hash, Profile: resolution.Name, Rows: rows}, nil
}

// Print writes the result as a fixed width table, the main human facing output
// of the replay tool.
func Print(w io.Writer, result *Result) error {
	fmt.Fprintf(w, "profile=%s configHash=%s\n", result.Profile, result.ConfigHash)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "NODE\tFIT\tPACK\tPENALTY\tSCORE\tNOTE"); err != nil {
		return err
	}
	for _, row := range result.Rows {
		note := ""
		if row.FitSkipped {
			note += "fit:skipped "
		}
		if row.PackSkipped {
			note += "pack:skipped "
		}
		if row.Err != "" {
			note += "error: " + row.Err
		}
		if _, err := fmt.Fprintf(tw, "%s\t%.4f\t%.4f\t%d\t%d\t%s\n",
			row.Node, row.Fit, row.Pack, row.Penalty, row.Score, note); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// Requests converts quantity strings into the scoring vector. Keys are "cpu",
// "memory" and the configured GPU resource name.
func Requests(in map[string]string, gpuResourceName string) (model.TaskRequests, error) {
	list, err := resourceList(in)
	if err != nil {
		return model.TaskRequests{}, err
	}
	return kubeadapter.ResourcesFromList(list, v1.ResourceName(gpuResourceName)), nil
}

// Resources converts quantity strings into a resource vector. gpuResourceName
// selects which scalar resource counts towards the GPU dimension.
func Resources(in map[string]string, gpuResourceName string) (model.Resources, error) {
	list, err := resourceList(in)
	if err != nil {
		return model.Resources{}, err
	}
	return kubeadapter.ResourcesFromList(list, v1.ResourceName(gpuResourceName)), nil
}

func resourceList(in map[string]string) (v1.ResourceList, error) {
	list := make(v1.ResourceList, len(in))
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		quantity, err := resource.ParseQuantity(in[key])
		if err != nil {
			return nil, fmt.Errorf("quantity %q: %w", key, err)
		}
		list[v1.ResourceName(key)] = quantity
	}
	return list, nil
}

func annotationFor(name string) map[string]string {
	if name == "" {
		return nil
	}
	return map[string]string{profile.AnnotationKey: name}
}
