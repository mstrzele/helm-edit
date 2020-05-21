package main

import (
	"log"
	"os"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/cli"
)

func main() {
	settings := cli.New()
	actionConfig := new(action.Configuration)
	err := actionConfig.Init(settings.RESTClientGetter(), settings.Namespace(), os.Getenv("HELM_DRIVER"), log.Printf)
	if err != nil {
		return
	}
	cmd := newEditCmd(actionConfig, os.Stdout)
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
