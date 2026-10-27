// Command tahrs-scheduler is an out-of-tree Kubernetes scheduler that decides
// Pod placement on heterogeneous node pools with the tahrs scoring plugin.
//
// The binary reuses the upstream scheduler command and only registers the
// plugin; the extension points, leader election and configuration handling are
// the real ones from k8s.io/kubernetes.
package main

import (
	"os"

	"k8s.io/component-base/cli"
	"k8s.io/kubernetes/cmd/kube-scheduler/app"

	"github.com/SakkijarvenPolka/tahrs-scheduler/internal/plugin/tahrs"
)

func main() {
	command := app.NewSchedulerCommand(app.WithPlugin(tahrs.Name, tahrs.New))
	os.Exit(cli.Run(command))
}
