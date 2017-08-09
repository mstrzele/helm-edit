package main

import (
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	"k8s.io/helm/pkg/chartutil"
	"k8s.io/helm/pkg/helm"
	"k8s.io/helm/pkg/proto/hapi/release"
)

type editCmd struct {
	release   string
	out       io.Writer
	client    helm.Interface
	allValues bool
	editor    string
	timeout   int64
	wait      bool
}

func newEditCmd(client helm.Interface, out io.Writer) *cobra.Command {

	edit := &editCmd{
		out:    out,
		client: client,
	}

	cmd := &cobra.Command{
		Use:     "edit [flags] RELEASE",
		Short:   fmt.Sprintf("edit a release"),
		PreRunE: setupConnection,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return fmt.Errorf("This command neeeds 1 argument: release name")
			}

			edit.release = args[0]
			edit.client = ensureHelmClient(edit.client)
			edit.editor = os.ExpandEnv(edit.editor)

			return edit.run()
		},
	}

	f := cmd.Flags()
	f.BoolVarP(&edit.allValues, "all", "a", false, "edit all (computed) vals")
	f.Int64Var(&edit.timeout, "timeout", 300, "time in seconds to wait for any individual Kubernetes operation (like Jobs for hooks)")
	f.BoolVar(&edit.wait, "wait", false, "if set, will wait until all Pods, PVCs, Services, and minimum number of Pods of a Deployment are in a ready state before marking the release as successful. It will wait for as long as --timeout")
	f.StringVarP(&edit.editor, "editor", "e", "$EDITOR", "name of the editor")

	return cmd
}

func (e *editCmd) vals(rel *release.Release) (string, error) {
	if e.allValues {
		cfg, err := chartutil.CoalesceValues(rel.Chart, rel.Config)
		if err != nil {
			return "", err
		}

		return cfg.YAML()
	} else {
		return rel.Config.Raw, nil
	}
}

func (e *editCmd) run() error {
	res, err := e.client.ReleaseContent(e.release)
	if err != nil {
		return err
	}

	tmpfile, err := ioutil.TempFile(os.TempDir(), fmt.Sprintf("helm-edit-%s", res.Release.Name))
	if err != nil {
		return err
	}

	defer os.Remove(tmpfile.Name())

	vals, err := e.vals(res.Release)
	if err != nil {
		return err
	}

	tmpfile.WriteString(vals)

	if err := tmpfile.Close(); err != nil {
		return err
	}

	editor := strings.Split(e.editor, " ")

	cmd := exec.Command(editor[0], append(editor[1:], tmpfile.Name())...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = e.out
	cmd.Stderr = e.out
	if err := cmd.Run(); err != nil {
		return err
	}

	rawVals, err := ioutil.ReadFile(tmpfile.Name())
	if err != nil {
		return err
	}

	if vals != string(rawVals) {
		_, err := e.client.UpdateReleaseFromChart(
			res.Release.Name,
			res.Release.Chart,
			helm.UpdateValueOverrides(rawVals),
			helm.UpgradeTimeout(e.timeout),
			helm.UpgradeWait(e.wait))
		if err != nil {
			return err
		}

		// TODO: print release

		fmt.Fprintf(e.out, "Release %q has been edited. Happy Helming!\n", e.release)

		// TODO: print the status like status command does
	}

	return nil
}
