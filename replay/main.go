// Command tahrsctl is the offline companion of tahrs-scheduler.
//
// replay scores a Pod against a JSON/YAML node snapshot using the same scoring
// library and configuration file as the scheduler, and prints the per-node
// breakdown. It needs no cluster and is the primary way to explain a placement
// decision.
//
//	tahrsctl replay --config tahrs-config.yaml --pod pod.yaml --nodes nodes.yaml
package main

import (
	"flag"
	"fmt"
	"os"

	"sigs.k8s.io/yaml"

	"github.com/SakkijarvenPolka/tahrs-scheduler/internal/config"
	"github.com/SakkijarvenPolka/tahrs-scheduler/internal/replay"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "replay" {
		fmt.Fprintln(os.Stderr, "usage: tahrsctl replay --config <config.yaml> --pod <pod.yaml> --nodes <nodes.yaml>")
		os.Exit(2)
	}

	flags := flag.NewFlagSet("replay", flag.ExitOnError)
	configPath := flags.String("config", "", "path to the tahrs configuration file")
	podPath := flags.String("pod", "", "path to the workload file (name, profile, requests)")
	nodesPath := flags.String("nodes", "", "path to the cluster snapshot (nodes)")
	if err := flags.Parse(os.Args[2:]); err != nil {
		os.Exit(2)
	}
	for name, value := range map[string]string{"config": *configPath, "pod": *podPath, "nodes": *nodesPath} {
		if value == "" {
			fmt.Fprintf(os.Stderr, "tahrsctl replay: --%s is required\n", name)
			os.Exit(2)
		}
	}

	loaded, err := config.LoadFile(*configPath)
	if err != nil {
		fail(err)
	}
	var workload replay.Workload
	if err := decodeFile(*podPath, &workload); err != nil {
		fail(err)
	}
	var cluster replay.Cluster
	if err := decodeFile(*nodesPath, &cluster); err != nil {
		fail(err)
	}

	result, err := replay.Table(loaded, workload, cluster)
	if err != nil {
		fail(err)
	}
	if err := replay.Print(os.Stdout, result); err != nil {
		fail(err)
	}
}

func decodeFile(path string, into any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %q: %w", path, err)
	}
	if err := yaml.UnmarshalStrict(data, into); err != nil {
		return fmt.Errorf("decode %q: %w", path, err)
	}
	return nil
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "tahrsctl: %v\n", err)
	os.Exit(1)
}
