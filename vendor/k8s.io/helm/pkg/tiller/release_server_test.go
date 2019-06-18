/*
Copyright The Helm Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package tiller

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/ghodss/yaml"
	"github.com/golang/protobuf/ptypes/timestamp"
	"github.com/technosophos/moniker"
	"golang.org/x/net/context"
	"google.golang.org/grpc/metadata"
	v1 "k8s.io/api/core/v1"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/kubernetes/fake"

	"k8s.io/helm/pkg/helm"
	"k8s.io/helm/pkg/hooks"
	"k8s.io/helm/pkg/kube"
	"k8s.io/helm/pkg/proto/hapi/chart"
	"k8s.io/helm/pkg/proto/hapi/release"
	"k8s.io/helm/pkg/proto/hapi/services"
	"k8s.io/helm/pkg/storage"
	"k8s.io/helm/pkg/storage/driver"
	"k8s.io/helm/pkg/tiller/environment"
)

const notesText = "my notes here"

var manifestWithHook = `kind: ConfigMap
metadata:
  name: test-cm
  annotations:
    "helm.sh/hook": post-install,pre-delete
data:
  name: value`

var manifestWithCRDHook = `
apiVersion: apiextensions.k8s.io/v1beta1
kind: CustomResourceDefinition
metadata:
  name: crontabs.stable.example.com
  annotations:
    "helm.sh/hook": crd-install
spec:
  group: stable.example.com
  version: v1
  scope: Namespaced
  names:
    plural: crontabs
    singular: crontab
    kind: CronTab
    shortNames:
    - ct
`

var manifestWithTestHook = `kind: Pod
metadata:
  name: finding-nemo,
  annotations:
    "helm.sh/hook": test-success
spec:
  containers:
  - name: nemo-test
    image: fake-image
    cmd: fake-command
`

var manifestWithKeep = `kind: ConfigMap
metadata:
  name: test-cm-keep-a
  annotations:
    "helm.sh/resource-policy": keep
data:
  name: value
`

var manifestWithKeepEmpty = `kind: ConfigMap
metadata:
  name: test-cm-keep-b
  annotations:
    "helm.sh/resource-policy": ""
data:
  name: value
`

var manifestWithUpgradeHooks = `kind: ConfigMap
metadata:
  name: test-cm
  annotations:
    "helm.sh/hook": post-upgrade,pre-upgrade
data:
  name: value`

var manifestWithRollbackHooks = `kind: ConfigMap
metadata:
  name: test-cm
  annotations:
    "helm.sh/hook": post-rollback,pre-rollback
data:
  name: value
`

type chartOptions struct {
	*chart.Chart
}

type chartOption func(*chartOptions)

func rsFixture() *ReleaseServer {
	return NewReleaseServer(MockEnvironment(), fake.NewSimpleClientset(), false)
}

func buildChart(opts ...chartOption) *chart.Chart {
	c := &chartOptions{
		Chart: &chart.Chart{
			// TODO: This should be more complete.
			Metadata: &chart.Metadata{
				Name: "hello",
			},
			// This adds a basic template and hooks.
			Templates: []*chart.Template{
				{Name: "templates/hello", Data: []byte("hello: world")},
				{Name: "templates/hooks", Data: []byte(manifestWithHook)},
			},
		},
	}

	for _, opt := range opts {
		opt(c)
	}

	return c.Chart
}

func withKube(version string) chartOption {
	return func(opts *chartOptions) {
		opts.Metadata.KubeVersion = version
	}
}

func withTiller(version string) chartOption {
	return func(opts *chartOptions) {
		opts.Metadata.TillerVersion = version
	}
}

func withDependency(dependencyOpts ...chartOption) chartOption {
	return func(opts *chartOptions) {
		opts.Dependencies = append(opts.Dependencies, buildChart(dependencyOpts...))
	}
}

func withNotes(notes string) chartOption {
	return func(opts *chartOptions) {
		opts.Templates = append(opts.Templates, &chart.Template{
			Name: "templates/NOTES.txt",
			Data: []byte(notes),
		})
	}
}

func withSampleTemplates() chartOption {
	return func(opts *chartOptions) {
		sampleTemplates := []*chart.Template{
			// This adds basic templates and partials.
			{Name: "templates/goodbye", Data: []byte("goodbye: world")},
			{Name: "templates/empty", Data: []byte("")},
			{Name: "templates/with-partials", Data: []byte(`hello: {{ template "_planet" . }}`)},
			{Name: "templates/partials/_planet", Data: []byte(`{{define "_planet"}}Earth{{end}}`)},
		}
		opts.Templates = append(opts.Templates, sampleTemplates...)
	}
}

type installOptions struct {
	*services.InstallReleaseRequest
}

type installOption func(*installOptions)

func withName(name string) installOption {
	return func(opts *installOptions) {
		opts.Name = name
	}
}

func withDryRun() installOption {
	return func(opts *installOptions) {
		opts.DryRun = true
	}
}

func withDisabledHooks() installOption {
	return func(opts *installOptions) {
		opts.DisableHooks = true
	}
}

func withReuseName() installOption {
	return func(opts *installOptions) {
		opts.ReuseName = true
	}
}

func withChart(chartOpts ...chartOption) installOption {
	return func(opts *installOptions) {
		opts.Chart = buildChart(chartOpts...)
	}
}

func withSubNotes() installOption {
	return func(opts *installOptions) {
		opts.SubNotes = true
	}
}

func installRequest(opts ...installOption) *services.InstallReleaseRequest {
	reqOpts := &installOptions{
		&services.InstallReleaseRequest{
			Namespace: "spaced",
			Chart:     buildChart(),
		},
	}

	for _, opt := range opts {
		opt(reqOpts)
	}

	return reqOpts.InstallReleaseRequest
}

// chartStub creates a fully stubbed out chart.
func chartStub() *chart.Chart {
	return buildChart(withSampleTemplates())
}

// releaseStub creates a release stub, complete with the chartStub as its chart.
func releaseStub() *release.Release {
	return namedReleaseStub("angry-panda", release.Status_DEPLOYED)
}

func namedReleaseStub(name string, status release.Status_Code) *release.Release {
	date := timestamp.Timestamp{Seconds: 242085845, Nanos: 0}
	return &release.Release{
		Name: name,
		Info: &release.Info{
			FirstDeployed: &date,
			LastDeployed:  &date,
			Status:        &release.Status{Code: status},
			Description:   "Named Release Stub",
		},
		Chart:   chartStub(),
		Config:  &chart.Config{Raw: `name: value`},
		Version: 1,
		Hooks: []*release.Hook{
			{
				Name:     "test-cm",
				Kind:     "ConfigMap",
				Path:     "test-cm",
				Manifest: manifestWithHook,
				Events: []release.Hook_Event{
					release.Hook_POST_INSTALL,
					release.Hook_PRE_DELETE,
				},
			},
			{
				Name:     "finding-nemo",
				Kind:     "Pod",
				Path:     "finding-nemo",
				Manifest: manifestWithTestHook,
				Events: []release.Hook_Event{
					release.Hook_RELEASE_TEST_SUCCESS,
				},
			},
		},
	}
}

func upgradeReleaseVersion(rel *release.Release) *release.Release {
	date := timestamp.Timestamp{Seconds: 242085845, Nanos: 0}

	rel.Info.Status.Code = release.Status_SUPERSEDED
	return &release.Release{
		Name: rel.Name,
		Info: &release.Info{
			FirstDeployed: rel.Info.FirstDeployed,
			LastDeployed:  &date,
			Status:        &release.Status{Code: release.Status_DEPLOYED},
		},
		Chart:   rel.Chart,
		Config:  rel.Config,
		Version: rel.Version + 1,
	}
}

func TestValidName(t *testing.T) {
	for name, valid := range map[string]error{
		"nina pinta santa-maria": errInvalidName,
		"nina-pinta-santa-maria": nil,
		"-nina":                  errInvalidName,
		"pinta-":                 errInvalidName,
		"santa-maria":            nil,
		"niña":                   errInvalidName,
		"...":                    errInvalidName,
		"pinta...":               errInvalidName,
		"santa...maria":          nil,
		"":                       errMissingRelease,
		" ":                      errInvalidName,
		".nina.":                 errInvalidName,
		"nina.pinta":             nil,
		"abcdefghi-abcdefghi-abcdefghi-abcdefghi-abcdefghi-abcd": errInvalidName,
	} {
		if valid != validateReleaseName(name) {
			t.Errorf("Expected %q to be %t", name, valid)
		}
	}
}

func TestGetVersionSet(t *testing.T) {
	rs := rsFixture()
	vs, err := GetVersionSet(rs.clientset.Discovery())
	if err != nil {
		t.Error(err)
	}
	if !vs.Has("v1") {
		t.Errorf("Expected supported versions to at least include v1.")
	}
	if vs.Has("nosuchversion/v1") {
		t.Error("Non-existent version is reported found.")
	}
}

func TestUniqName(t *testing.T) {
	rs := rsFixture()

	rel1 := releaseStub()
	rel2 := releaseStub()
	rel2.Name = "happy-panda"
	rel2.Info.Status.Code = release.Status_DELETED

	rs.env.Releases.Create(rel1)
	rs.env.Releases.Create(rel2)

	tests := []struct {
		name   string
		expect string
		reuse  bool
		err    bool
	}{
		{"first", "first", false, false},
		{"", "[a-z]+-[a-z]+", false, false},
		{"angry-panda", "", false, true},
		{"happy-panda", "", false, true},
		{"happy-panda", "happy-panda", true, false},
		{"hungry-hungry-hungry-hungry-hungry-hungry-hungry-hungry-hippos", "", true, true}, // Exceeds max name length
	}

	for _, tt := range tests {
		u, err := rs.uniqName(tt.name, tt.reuse)
		if err != nil {
			if tt.err {
				continue
			}
			t.Fatal(err)
		}
		if tt.err {
			t.Errorf("Expected an error for %q", tt.name)
		}
		if match, err := regexp.MatchString(tt.expect, u); err != nil {
			t.Fatal(err)
		} else if !match {
			t.Errorf("Expected %q to match %q", u, tt.expect)
		}
	}
}

type fakeNamer struct {
	name string
}

func NewFakeNamer(nam string) moniker.Namer {
	return &fakeNamer{
		name: nam,
	}
}

func (f *fakeNamer) Name() string {
	return f.NameSep(" ")
}

func (f *fakeNamer) NameSep(sep string) string {
	return f.name
}

func TestCreateUniqueName(t *testing.T) {
	rs := rsFixture()

	rel1 := releaseStub()
	rel1.Name = "happy-panda"

	rs.env.Releases.Create(rel1)

	tests := []struct {
		name   string
		expect string
		err    bool
	}{
		{"happy-panda", "ERROR", true},
		{"wobbly-octopus", "[a-z]+-[a-z]+", false},
	}

	for _, tt := range tests {
		m := NewFakeNamer(tt.name)
		u, err := rs.createUniqName(m)
		if err != nil {
			if tt.err {
				continue
			}
			t.Fatal(err)
		}
		if tt.err {
			t.Errorf("Expected an error for %q", tt.name)
		}
		if match, err := regexp.MatchString(tt.expect, u); err != nil {
			t.Fatal(err)
		} else if !match {
			t.Errorf("Expected %q to match %q", u, tt.expect)
		}
	}

}

func releaseWithKeepStub(rlsName string) *release.Release {
	ch := &chart.Chart{
		Metadata: &chart.Metadata{
			Name: "bunnychart",
		},
		Templates: []*chart.Template{
			{Name: "templates/configmap-keep-a", Data: []byte(manifestWithKeep)},
			{Name: "templates/configmap-keep-b", Data: []byte(manifestWithKeepEmpty)},
		},
	}

	date := timestamp.Timestamp{Seconds: 242085845, Nanos: 0}
	rl := &release.Release{
		Name: rlsName,
		Info: &release.Info{
			FirstDeployed: &date,
			LastDeployed:  &date,
			Status:        &release.Status{Code: release.Status_DEPLOYED},
		},
		Chart:   ch,
		Config:  &chart.Config{Raw: `name: value`},
		Version: 1,
	}

	helm.RenderReleaseMock(rl, false)

	return rl
}

func MockEnvironment() *environment.Environment {
	e := environment.New()
	e.Releases = storage.Init(driver.NewMemory())
	e.KubeClient = &environment.PrintingKubeClient{Out: ioutil.Discard}
	return e
}

func newUpdateFailingKubeClient() *updateFailingKubeClient {
	return &updateFailingKubeClient{
		PrintingKubeClient: environment.PrintingKubeClient{Out: os.Stdout},
	}

}

type updateFailingKubeClient struct {
	environment.PrintingKubeClient
}

func (u *updateFailingKubeClient) Update(namespace string, originalReader, modifiedReader io.Reader, force bool, recreate bool, timeout int64, shouldWait bool) error {
	return u.UpdateWithOptions(namespace, originalReader, modifiedReader, kube.UpdateOptions{
		Force:      force,
		Recreate:   recreate,
		Timeout:    timeout,
		ShouldWait: shouldWait,
	})
}

func (u *updateFailingKubeClient) UpdateWithOptions(namespace string, originalReader, modifiedReader io.Reader, opts kube.UpdateOptions) error {
	return errors.New("Failed update in kube client")
}

func newHookFailingKubeClient() *hookFailingKubeClient {
	return &hookFailingKubeClient{
		PrintingKubeClient: environment.PrintingKubeClient{Out: ioutil.Discard},
	}
}

type hookFailingKubeClient struct {
	environment.PrintingKubeClient
}

func (h *hookFailingKubeClient) WatchUntilReady(ns string, r io.Reader, timeout int64, shouldWait bool) error {
	return errors.New("Failed watch")
}

func newDeleteFailingKubeClient() *deleteFailingKubeClient {
	return &deleteFailingKubeClient{
		PrintingKubeClient: environment.PrintingKubeClient{Out: ioutil.Discard},
	}
}

type deleteFailingKubeClient struct {
	environment.PrintingKubeClient
}

func (d *deleteFailingKubeClient) Delete(ns string, r io.Reader) error {
	return kube.ErrNoObjectsVisited
}

type mockListServer struct {
	val *services.ListReleasesResponse
}

func (l *mockListServer) Send(res *services.ListReleasesResponse) error {
	l.val = res
	return nil
}

func (l *mockListServer) Context() context.Context       { return helm.NewContext() }
func (l *mockListServer) SendMsg(v interface{}) error    { return nil }
func (l *mockListServer) RecvMsg(v interface{}) error    { return nil }
func (l *mockListServer) SendHeader(m metadata.MD) error { return nil }
func (l *mockListServer) SetTrailer(m metadata.MD)       {}
func (l *mockListServer) SetHeader(m metadata.MD) error  { return nil }

type mockRunReleaseTestServer struct{}

func (rs mockRunReleaseTestServer) Send(m *services.TestReleaseResponse) error {
	return nil
}
func (rs mockRunReleaseTestServer) SetHeader(m metadata.MD) error  { return nil }
func (rs mockRunReleaseTestServer) SendHeader(m metadata.MD) error { return nil }
func (rs mockRunReleaseTestServer) SetTrailer(m metadata.MD)       {}
func (rs mockRunReleaseTestServer) SendMsg(v interface{}) error    { return nil }
func (rs mockRunReleaseTestServer) RecvMsg(v interface{}) error    { return nil }
func (rs mockRunReleaseTestServer) Context() context.Context       { return helm.NewContext() }

type mockHooksManifest struct {
	Metadata struct {
		Name        string
		Annotations map[string]string
	}
}
type mockHooksKubeClient struct {
	Resources map[string]*mockHooksManifest
}

var errResourceExists = errors.New("resource already exists")

func (kc *mockHooksKubeClient) makeManifest(r io.Reader) (*mockHooksManifest, error) {
	b, err := ioutil.ReadAll(r)
	if err != nil {
		return nil, err
	}

	manifest := &mockHooksManifest{}
	err = yaml.Unmarshal(b, manifest)
	if err != nil {
		return nil, err
	}

	return manifest, nil
}
func (kc *mockHooksKubeClient) Create(ns string, r io.Reader, timeout int64, shouldWait bool) error {
	manifest, err := kc.makeManifest(r)
	if err != nil {
		return err
	}

	if _, hasKey := kc.Resources[manifest.Metadata.Name]; hasKey {
		return errResourceExists
	}

	kc.Resources[manifest.Metadata.Name] = manifest

	return nil
}
func (kc *mockHooksKubeClient) Get(ns string, r io.Reader) (string, error) {
	return "", nil
}
func (kc *mockHooksKubeClient) Delete(ns string, r io.Reader) error {
	manifest, err := kc.makeManifest(r)
	if err != nil {
		return err
	}

	delete(kc.Resources, manifest.Metadata.Name)

	return nil
}
func (kc *mockHooksKubeClient) WatchUntilReady(ns string, r io.Reader, timeout int64, shouldWait bool) error {
	paramManifest, err := kc.makeManifest(r)
	if err != nil {
		return err
	}

	manifest, hasManifest := kc.Resources[paramManifest.Metadata.Name]
	if !hasManifest {
		return fmt.Errorf("mockHooksKubeClient.WatchUntilReady: no such resource %s found", paramManifest.Metadata.Name)
	}

	if manifest.Metadata.Annotations["mockHooksKubeClient/Emulate"] == "hook-failed" {
		return fmt.Errorf("mockHooksKubeClient.WatchUntilReady: hook-failed")
	}

	return nil
}
func (kc *mockHooksKubeClient) Update(ns string, currentReader, modifiedReader io.Reader, force bool, recreate bool, timeout int64, shouldWait bool) error {
	return nil
}
func (kc *mockHooksKubeClient) UpdateWithOptions(ns string, currentReader, modifiedReader io.Reader, opts kube.UpdateOptions) error {
	return nil
}
func (kc *mockHooksKubeClient) Build(ns string, reader io.Reader) (kube.Result, error) {
	return []*resource.Info{}, nil
}
func (kc *mockHooksKubeClient) BuildUnstructured(ns string, reader io.Reader) (kube.Result, error) {
	return []*resource.Info{}, nil
}
func (kc *mockHooksKubeClient) Validate(ns string, reader io.Reader) error {
	return nil
}
func (kc *mockHooksKubeClient) WaitAndGetCompletedPodPhase(namespace string, reader io.Reader, timeout time.Duration) (v1.PodPhase, error) {
	return v1.PodUnknown, nil
}

func (kc *mockHooksKubeClient) WaitUntilCRDEstablished(reader io.Reader, timeout time.Duration) error {
	return nil
}

func deletePolicyStub(kubeClient *mockHooksKubeClient) *ReleaseServer {
	e := environment.New()
	e.Releases = storage.Init(driver.NewMemory())
	e.KubeClient = kubeClient

	clientset := fake.NewSimpleClientset()
	return &ReleaseServer{
		ReleaseModule: &LocalReleaseModule{
			clientset: clientset,
		},
		env:       e,
		clientset: clientset,
		Log:       func(_ string, _ ...interface{}) {},
	}
}

func deletePolicyHookStub(hookName string, extraAnnotations map[string]string, DeletePolicies []release.Hook_DeletePolicy) *release.Hook {
	extraAnnotationsStr := ""
	for k, v := range extraAnnotations {
		extraAnnotationsStr += fmt.Sprintf("    \"%s\": \"%s\"\n", k, v)
	}

	return &release.Hook{
		Name: hookName,
		Kind: "Job",
		Path: hookName,
		Manifest: fmt.Sprintf(`kind: Job
metadata:
  name: %s
  annotations:
    "helm.sh/hook": pre-install,pre-upgrade
%sdata:
name: value`, hookName, extraAnnotationsStr),
		Events: []release.Hook_Event{
			release.Hook_PRE_INSTALL,
			release.Hook_PRE_UPGRADE,
		},
		DeletePolicies: DeletePolicies,
	}
}

func execHookShouldSucceed(rs *ReleaseServer, hook *release.Hook, releaseName string, namespace string, hookType string) error {
	err := rs.execHook([]*release.Hook{hook}, releaseName, namespace, hookType, 600)
	if err != nil {
		return fmt.Errorf("expected hook %s to be successful: %s", hook.Name, err)
	}
	return nil
}

func execHookShouldFail(rs *ReleaseServer, hook *release.Hook, releaseName string, namespace string, hookType string) error {
	err := rs.execHook([]*release.Hook{hook}, releaseName, namespace, hookType, 600)
	if err == nil {
		return fmt.Errorf("expected hook %s to be failed", hook.Name)
	}
	return nil
}

func execHookShouldFailWithError(rs *ReleaseServer, hook *release.Hook, releaseName string, namespace string, hookType string, expectedError error) error {
	err := rs.execHook([]*release.Hook{hook}, releaseName, namespace, hookType, 600)
	if err != expectedError {
		return fmt.Errorf("expected hook %s to fail with error %v, got %v", hook.Name, expectedError, err)
	}
	return nil
}

type deletePolicyContext struct {
	ReleaseServer *ReleaseServer
	ReleaseName   string
	Namespace     string
	HookName      string
	KubeClient    *mockHooksKubeClient
}

func newDeletePolicyContext() *deletePolicyContext {
	kubeClient := &mockHooksKubeClient{
		Resources: make(map[string]*mockHooksManifest),
	}

	return &deletePolicyContext{
		KubeClient:    kubeClient,
		ReleaseServer: deletePolicyStub(kubeClient),
		ReleaseName:   "flying-carp",
		Namespace:     "river",
		HookName:      "migration-job",
	}
}

func TestSuccessfulHookWithoutDeletePolicy(t *testing.T) {
	ctx := newDeletePolicyContext()
	hook := deletePolicyHookStub(ctx.HookName, nil, nil)

	err := execHookShouldSucceed(ctx.ReleaseServer, hook, ctx.ReleaseName, ctx.Namespace, hooks.PreInstall)
	if err != nil {
		t.Error(err)
	}
	if _, hasResource := ctx.KubeClient.Resources[hook.Name]; !hasResource {
		t.Errorf("expected resource %s to be created by kube client", hook.Name)
	}
}

func TestFailedHookWithoutDeletePolicy(t *testing.T) {
	ctx := newDeletePolicyContext()
	hook := deletePolicyHookStub(ctx.HookName,
		map[string]string{"mockHooksKubeClient/Emulate": "hook-failed"},
		nil,
	)

	err := execHookShouldFail(ctx.ReleaseServer, hook, ctx.ReleaseName, ctx.Namespace, hooks.PreInstall)
	if err != nil {
		t.Error(err)
	}
	if _, hasResource := ctx.KubeClient.Resources[hook.Name]; !hasResource {
		t.Errorf("expected resource %s to be created by kube client", hook.Name)
	}
}

func TestSuccessfulHookWithSucceededDeletePolicy(t *testing.T) {
	ctx := newDeletePolicyContext()
	hook := deletePolicyHookStub(ctx.HookName,
		map[string]string{"helm.sh/hook-delete-policy": "hook-succeeded"},
		[]release.Hook_DeletePolicy{release.Hook_SUCCEEDED},
	)

	err := execHookShouldSucceed(ctx.ReleaseServer, hook, ctx.ReleaseName, ctx.Namespace, hooks.PreInstall)
	if err != nil {
		t.Error(err)
	}
	if _, hasResource := ctx.KubeClient.Resources[hook.Name]; hasResource {
		t.Errorf("expected resource %s to be unexisting after hook succeeded", hook.Name)
	}
}

func TestSuccessfulHookWithFailedDeletePolicy(t *testing.T) {
	ctx := newDeletePolicyContext()
	hook := deletePolicyHookStub(ctx.HookName,
		map[string]string{"helm.sh/hook-delete-policy": "hook-failed"},
		[]release.Hook_DeletePolicy{release.Hook_FAILED},
	)

	err := execHookShouldSucceed(ctx.ReleaseServer, hook, ctx.ReleaseName, ctx.Namespace, hooks.PreInstall)
	if err != nil {
		t.Error(err)
	}
	if _, hasResource := ctx.KubeClient.Resources[hook.Name]; !hasResource {
		t.Errorf("expected resource %s to be existing after hook succeeded", hook.Name)
	}
}

func TestFailedHookWithSucceededDeletePolicy(t *testing.T) {
	ctx := newDeletePolicyContext()

	hook := deletePolicyHookStub(ctx.HookName,
		map[string]string{
			"mockHooksKubeClient/Emulate": "hook-failed",
			"helm.sh/hook-delete-policy":  "hook-succeeded",
		},
		[]release.Hook_DeletePolicy{release.Hook_SUCCEEDED},
	)

	err := execHookShouldFail(ctx.ReleaseServer, hook, ctx.ReleaseName, ctx.Namespace, hooks.PreInstall)
	if err != nil {
		t.Error(err)
	}
	if _, hasResource := ctx.KubeClient.Resources[hook.Name]; !hasResource {
		t.Errorf("expected resource %s to be existing after hook failed", hook.Name)
	}
}

func TestFailedHookWithFailedDeletePolicy(t *testing.T) {
	ctx := newDeletePolicyContext()

	hook := deletePolicyHookStub(ctx.HookName,
		map[string]string{
			"mockHooksKubeClient/Emulate": "hook-failed",
			"helm.sh/hook-delete-policy":  "hook-failed",
		},
		[]release.Hook_DeletePolicy{release.Hook_FAILED},
	)

	err := execHookShouldFail(ctx.ReleaseServer, hook, ctx.ReleaseName, ctx.Namespace, hooks.PreInstall)
	if err != nil {
		t.Error(err)
	}
	if _, hasResource := ctx.KubeClient.Resources[hook.Name]; hasResource {
		t.Errorf("expected resource %s to be unexisting after hook failed", hook.Name)
	}
}

func TestSuccessfulHookWithSuccededOrFailedDeletePolicy(t *testing.T) {
	ctx := newDeletePolicyContext()

	hook := deletePolicyHookStub(ctx.HookName,
		map[string]string{
			"helm.sh/hook-delete-policy": "hook-succeeded,hook-failed",
		},
		[]release.Hook_DeletePolicy{release.Hook_SUCCEEDED, release.Hook_FAILED},
	)

	err := execHookShouldSucceed(ctx.ReleaseServer, hook, ctx.ReleaseName, ctx.Namespace, hooks.PreInstall)
	if err != nil {
		t.Error(err)
	}
	if _, hasResource := ctx.KubeClient.Resources[hook.Name]; hasResource {
		t.Errorf("expected resource %s to be unexisting after hook succeeded", hook.Name)
	}
}

func TestFailedHookWithSuccededOrFailedDeletePolicy(t *testing.T) {
	ctx := newDeletePolicyContext()

	hook := deletePolicyHookStub(ctx.HookName,
		map[string]string{
			"mockHooksKubeClient/Emulate": "hook-failed",
			"helm.sh/hook-delete-policy":  "hook-succeeded,hook-failed",
		},
		[]release.Hook_DeletePolicy{release.Hook_SUCCEEDED, release.Hook_FAILED},
	)

	err := execHookShouldFail(ctx.ReleaseServer, hook, ctx.ReleaseName, ctx.Namespace, hooks.PreInstall)
	if err != nil {
		t.Error(err)
	}
	if _, hasResource := ctx.KubeClient.Resources[hook.Name]; hasResource {
		t.Errorf("expected resource %s to be unexisting after hook failed", hook.Name)
	}
}

func TestHookAlreadyExists(t *testing.T) {
	ctx := newDeletePolicyContext()

	hook := deletePolicyHookStub(ctx.HookName, nil, nil)

	err := execHookShouldSucceed(ctx.ReleaseServer, hook, ctx.ReleaseName, ctx.Namespace, hooks.PreInstall)
	if err != nil {
		t.Error(err)
	}

	if _, hasResource := ctx.KubeClient.Resources[hook.Name]; !hasResource {
		t.Errorf("expected resource %s to be existing after hook succeeded", hook.Name)
	}

	err = execHookShouldFailWithError(ctx.ReleaseServer, hook, ctx.ReleaseName, ctx.Namespace, hooks.PreUpgrade, errResourceExists)
	if err != nil {
		t.Error(err)
	}

	if _, hasResource := ctx.KubeClient.Resources[hook.Name]; !hasResource {
		t.Errorf("expected resource %s to be existing after already exists error", hook.Name)
	}
}

func TestHookDeletingWithBeforeHookCreationDeletePolicy(t *testing.T) {
	ctx := newDeletePolicyContext()

	hook := deletePolicyHookStub(ctx.HookName,
		map[string]string{"helm.sh/hook-delete-policy": "before-hook-creation"},
		[]release.Hook_DeletePolicy{release.Hook_BEFORE_HOOK_CREATION},
	)

	err := execHookShouldSucceed(ctx.ReleaseServer, hook, ctx.ReleaseName, ctx.Namespace, hooks.PreInstall)
	if err != nil {
		t.Error(err)
	}

	if _, hasResource := ctx.KubeClient.Resources[hook.Name]; !hasResource {
		t.Errorf("expected resource %s to be existing after hook succeeded", hook.Name)
	}

	err = execHookShouldSucceed(ctx.ReleaseServer, hook, ctx.ReleaseName, ctx.Namespace, hooks.PreUpgrade)
	if err != nil {
		t.Error(err)
	}

	if _, hasResource := ctx.KubeClient.Resources[hook.Name]; !hasResource {
		t.Errorf("expected resource %s to be existing after hook succeeded", hook.Name)
	}
}

func TestSuccessfulHookWithMixedDeletePolicies(t *testing.T) {
	ctx := newDeletePolicyContext()

	hook := deletePolicyHookStub(ctx.HookName,
		map[string]string{
			"helm.sh/hook-delete-policy": "hook-succeeded,before-hook-creation",
		},
		[]release.Hook_DeletePolicy{release.Hook_SUCCEEDED, release.Hook_BEFORE_HOOK_CREATION},
	)

	err := execHookShouldSucceed(ctx.ReleaseServer, hook, ctx.ReleaseName, ctx.Namespace, hooks.PreInstall)
	if err != nil {
		t.Error(err)
	}

	if _, hasResource := ctx.KubeClient.Resources[hook.Name]; hasResource {
		t.Errorf("expected resource %s to be unexisting after hook succeeded", hook.Name)
	}

	err = execHookShouldSucceed(ctx.ReleaseServer, hook, ctx.ReleaseName, ctx.Namespace, hooks.PreUpgrade)
	if err != nil {
		t.Error(err)
	}

	if _, hasResource := ctx.KubeClient.Resources[hook.Name]; hasResource {
		t.Errorf("expected resource %s to be unexisting after hook succeeded", hook.Name)
	}
}

func TestFailedHookWithMixedDeletePolicies(t *testing.T) {
	ctx := newDeletePolicyContext()

	hook := deletePolicyHookStub(ctx.HookName,
		map[string]string{
			"mockHooksKubeClient/Emulate": "hook-failed",
			"helm.sh/hook-delete-policy":  "hook-succeeded,before-hook-creation",
		},
		[]release.Hook_DeletePolicy{release.Hook_SUCCEEDED, release.Hook_BEFORE_HOOK_CREATION},
	)

	err := execHookShouldFail(ctx.ReleaseServer, hook, ctx.ReleaseName, ctx.Namespace, hooks.PreInstall)
	if err != nil {
		t.Error(err)
	}

	if _, hasResource := ctx.KubeClient.Resources[hook.Name]; !hasResource {
		t.Errorf("expected resource %s to be existing after hook failed", hook.Name)
	}

	err = execHookShouldFail(ctx.ReleaseServer, hook, ctx.ReleaseName, ctx.Namespace, hooks.PreUpgrade)
	if err != nil {
		t.Error(err)
	}

	if _, hasResource := ctx.KubeClient.Resources[hook.Name]; !hasResource {
		t.Errorf("expected resource %s to be existing after hook failed", hook.Name)
	}
}

func TestFailedThenSuccessfulHookWithMixedDeletePolicies(t *testing.T) {
	ctx := newDeletePolicyContext()

	hook := deletePolicyHookStub(ctx.HookName,
		map[string]string{
			"mockHooksKubeClient/Emulate": "hook-failed",
			"helm.sh/hook-delete-policy":  "hook-succeeded,before-hook-creation",
		},
		[]release.Hook_DeletePolicy{release.Hook_SUCCEEDED, release.Hook_BEFORE_HOOK_CREATION},
	)

	err := execHookShouldFail(ctx.ReleaseServer, hook, ctx.ReleaseName, ctx.Namespace, hooks.PreInstall)
	if err != nil {
		t.Error(err)
	}

	if _, hasResource := ctx.KubeClient.Resources[hook.Name]; !hasResource {
		t.Errorf("expected resource %s to be existing after hook failed", hook.Name)
	}

	hook = deletePolicyHookStub(ctx.HookName,
		map[string]string{
			"helm.sh/hook-delete-policy": "hook-succeeded,before-hook-creation",
		},
		[]release.Hook_DeletePolicy{release.Hook_SUCCEEDED, release.Hook_BEFORE_HOOK_CREATION},
	)

	err = execHookShouldSucceed(ctx.ReleaseServer, hook, ctx.ReleaseName, ctx.Namespace, hooks.PreUpgrade)
	if err != nil {
		t.Error(err)
	}

	if _, hasResource := ctx.KubeClient.Resources[hook.Name]; hasResource {
		t.Errorf("expected resource %s to be unexisting after hook succeeded", hook.Name)
	}
}
