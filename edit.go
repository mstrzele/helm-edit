package main

import (
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/release"
	"sigs.k8s.io/yaml"
)

var editHelp = `
This command return the values of given helm release in a text editor, and redploy the chart with the edited values.
`

type editCmd struct {
	release                    string
	out                        io.Writer
	cfg                        *action.Configuration
	allValues                  bool
	editor                     string
	timeout                    time.Duration
	wait                       bool
	revision                   int
	disableDefaultIntersection bool
}

func newEditCmd(cfg *action.Configuration, out io.Writer) *cobra.Command {

	edit := &editCmd{
		out: out}

	cmd := &cobra.Command{
		Use:   "edit [flags] RELEASE",
		Short: fmt.Sprintf("edit a release"),
		Long:  editHelp,
		RunE: func(cmd *cobra.Command, args []string) error {
			edit.cfg = cfg
			edit.editor = os.ExpandEnv(edit.editor)
			if len(args) != 1 {
				return fmt.Errorf("This command neeeds 1 argument: release name")
			}
			edit.release = args[0]
			return edit.run()
		},
	}
	f := cmd.Flags()
	f.BoolVarP(&edit.allValues, "all", "a", false, "edit all (computed) vals")
	f.IntVar(&edit.revision, "revision", 0, "edit the current chart with values from old revision")
	f.DurationVar(&edit.timeout, "timeout", 300*time.Second, "time to wait for any individual Kubernetes operation (like Jobs for hooks)")
	f.BoolVar(&edit.wait, "wait", false, "if set, will wait until all Pods, PVCs, Services, and minimum number of Pods of a Deployment are in a ready state before marking the release as successful. It will wait for as long as --timeout")
	f.StringVarP(&edit.editor, "editor", "e", "$EDITOR", "name of the editor")
	f.BoolVarP(&edit.disableDefaultIntersection, "disable-default-intersection", "m", false, "If set, user supplied values that are the same as default values won't be \"removed\" from user supplied values (careful using with -a, setting both of this params won't \"merge\" the values and all values will become user supplied values)")

	return cmd
}

func (e *editCmd) vals(rel *release.Release) (map[string]interface{}, error) {
	values := action.NewGetValues(e.cfg)
	values.AllValues = e.allValues
	values.Version = e.revision
	return values.Run(rel.Name)
}

func (e *editCmd) getDefaultsIntersection(overridesMap map[string]interface{}, defaultMap map[string]interface{}) map[string]interface{} {
	newMap := map[string]interface{}{}
	for key, value := range overridesMap {
		if _, ok := defaultMap[key]; ok {
			if !reflect.DeepEqual(value, defaultMap[key]) {
				innerMap, ok2 := value.(map[string]interface{})
				innerMapDefault, ok2Default := defaultMap[key].(map[string]interface{})
				if ok2 && ok2Default {
					newMap[key] = e.getDefaultsIntersection(innerMap, innerMapDefault)
				} else {
					newMap[key] = value
				}
			}
		} else {
			newMap[key] = value
		}
	}
	return newMap
}

func (e *editCmd) run() error {

	getRelease := action.NewGet(e.cfg)
	// getRelease.Version = 0
	res, err := getRelease.Run(e.release)
	if err != nil {
		return err
	}

	tmpfile, err := ioutil.TempFile(os.TempDir(), fmt.Sprintf("helm-edit-%s", res.Name))
	if err != nil {
		return err
	}

	defer os.Remove(tmpfile.Name())

	valsMap, err := e.vals(res)
	if err != nil {
		return err
	}
	vals, _ := yaml.Marshal(valsMap)
	tmpfile.Write(vals)

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

	newValues, err := chartutil.ReadValuesFile(tmpfile.Name())
	if err != nil {
		return err
	}
	if err != nil {
		return err
	}
	var userSuppliedVal map[string]interface{}
	if e.disableDefaultIntersection {
		userSuppliedVal = newValues.AsMap()
	} else {
		userSuppliedVal = e.getDefaultsIntersection(newValues.AsMap(), res.Chart.Values)
	}

	newValuesString, err := newValues.YAML()
	if err != nil {
		return err
	}
	if string(vals) != newValuesString {
		upgrade := action.NewUpgrade(e.cfg)
		upgrade.Wait = e.wait
		upgrade.Timeout = e.timeout
		res, err := upgrade.Run(
			res.Name,
			res.Chart,
			userSuppliedVal)
		if err != nil {
			return err
		}

		fmt.Fprintf(e.out, "Release %q has been edited. Happy Helming!\n%s", e.release, res.Info.Notes)

		// TODO: print the status like status command does
	} else {
		fmt.Fprintln(e.out, "Edit cancelled, no changes made!")
	}

	return nil
}
