/*
Copyright 2026 Trustle, Inc..

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

package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/zapr"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	dbosv1alpha1 "github.com/trustle/dbos-version-controller/api/v1alpha1"
	"github.com/trustle/dbos-version-controller/internal/config"
	// +kubebuilder:scaffold:imports
)

const (
	desiredVersion = "v20260916135248"
	desiredImage   = "registry.example.com/app:8332cd5"
	oldVersion     = "v20260901120000"
	otherVersion   = "v20260902120000"
	setName        = "dbos-worker"
	appNameLabel   = "app.kubernetes.io/name"
	appName        = "dbos-worker"
)

// now is the instant every test reconciles at.
var now = time.Date(2026, 9, 16, 14, 0, 0, 0, time.UTC)

const nowStamp = "2026-09-16T14:00:00Z"

const deploymentTemplate = `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: dbos-worker-__VERSION__
  labels:
    dbos-app-version: __VERSION__
    app.kubernetes.io/managed-by: dbos-version-controller
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: dbos-worker
      dbos-app-version: __VERSION__
  template:
    metadata:
      labels:
        app.kubernetes.io/name: dbos-worker
        dbos-app-version: __VERSION__
    spec:
      containers:
        - name: app
          image: __IMAGE__
          env:
            - name: DBOS_APPLICATION_VERSION
              value: __VERSION__
`

const scaledObjectTemplate = `
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: dbos-worker-__VERSION__
  labels:
    dbos-app-version: __VERSION__
    app.kubernetes.io/managed-by: dbos-version-controller
spec:
  scaleTargetRef:
    name: dbos-worker-__VERSION__
  minReplicaCount: 1
  triggers:
    - type: postgresql
      metricType: AverageValue
      authenticationRef:
        name: dbos-postgres-auth
      metadata:
        query: "SELECT 1 FROM dbos.workflow_status WHERE application_version = '__VERSION__'"
        targetQueryValue: "4"
    - type: postgresql
      metricType: AverageValue
      authenticationRef:
        name: dbos-postgres-auth
      metadata:
        query: "SELECT 2 FROM dbos.workflow_status WHERE application_version = '__VERSION__'"
        targetQueryValue: "2"
`

const pdbTemplate = `
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: dbos-worker-__VERSION__
  labels:
    dbos-app-version: __VERSION__
    app.kubernetes.io/managed-by: dbos-version-controller
spec:
  maxUnavailable: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: dbos-worker
      dbos-app-version: __VERSION__
`

var (
	testEnv    *envtest.Environment
	restCfg    *rest.Config
	k8sClient  client.Client
	namespaces atomic.Int64
)

func TestMain(m *testing.M) {
	logf.SetLogger(zapr.NewLogger(zap.NewNop()))

	if err := dbosv1alpha1.AddToScheme(scheme.Scheme); err != nil {
		fmt.Fprintf(os.Stderr, "add the WorkerVersionSet scheme: %v\n", err)
		os.Exit(1)
	}

	testEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "config", "crd", "bases"),
			filepath.Join("..", "..", "test", "crds"),
		},
		ErrorIfCRDPathMissing: true,
	}
	if directory := firstEnvTestBinaryDir(); directory != "" {
		testEnv.BinaryAssetsDirectory = directory
	}

	var err error
	restCfg, err = testEnv.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "start the test environment: %v\n", err)
		os.Exit(1)
	}

	k8sClient, err = client.New(restCfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		fmt.Fprintf(os.Stderr, "build the test client: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()

	if err := testEnv.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "stop the test environment: %v\n", err)
	}
	os.Exit(code)
}

// firstEnvTestBinaryDir locates the envtest binaries when KUBEBUILDER_ASSETS is unset, so the
// suite also runs from an IDE after 'make setup-envtest'.
func firstEnvTestBinaryDir() string {
	basePath := filepath.Join("..", "..", "bin", "k8s")
	entries, err := os.ReadDir(basePath)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return filepath.Join(basePath, entry.Name())
		}
	}
	return ""
}

// fixture is one test's namespace, reconciler and recorded log.
type fixture struct {
	t          *testing.T
	ctx        context.Context
	namespace  string
	reconciler *WorkerVersionSetReconciler
	logs       *observer.ObservedLogs

	// clock is the instant the next reconcile runs at.
	clock time.Time
	// rows answers the injected outstanding counter, keyed by version.
	rows map[string]int64
	// countErr fails the injected outstanding counter when set.
	countErr error
}

// newFixture starts one test in a namespace of its own.
func newFixture(t *testing.T, options ...func(*config.Config)) *fixture {
	t.Helper()

	namespace := fmt.Sprintf("dbos-workers-%d", namespaces.Add(1))
	ctx := context.Background()
	if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
		t.Fatalf("create the test namespace: %v", err)
	}

	settings := config.Default()
	settings.Namespace = namespace
	settings.DatabaseURL = "postgresql://user:pass@localhost:5432/dbos"
	for _, option := range options {
		option(&settings)
	}

	core, logs := observer.New(zap.InfoLevel)
	f := &fixture{
		t:         t,
		namespace: namespace,
		logs:      logs,
		clock:     now,
		rows:      map[string]int64{},
	}
	f.ctx = logf.IntoContext(ctx, zapr.NewLogger(zap.New(core)))
	f.reconciler = &WorkerVersionSetReconciler{
		Client: k8sClient,
		Scheme: scheme.Scheme,
		Config: settings,
		Clock:  func() time.Time { return f.clock },
		Outstanding: func(_ context.Context, version string) (int64, error) {
			if f.countErr != nil {
				return 0, f.countErr
			}
			return f.rows[version], nil
		},
	}
	return f
}

// dryRun turns on DVC_DRY_RUN for one fixture.
func dryRun(settings *config.Config) { settings.DryRun = true }

// reconcile runs one reconcile of the named set.
func (f *fixture) reconcile(name string) {
	f.t.Helper()
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: f.namespace, Name: name}}
	if _, err := f.reconciler.Reconcile(f.ctx, request); err != nil {
		f.t.Fatalf("reconcile %s: %v", name, err)
	}
}

// createSet writes one WorkerVersionSet, returning it.
func (f *fixture) createSet(
	name, version string,
	mutate ...func(*dbosv1alpha1.WorkerVersionSet),
) *dbosv1alpha1.WorkerVersionSet {
	f.t.Helper()
	set := &dbosv1alpha1.WorkerVersionSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.namespace},
		Spec: dbosv1alpha1.WorkerVersionSetSpec{
			Version: version,
			Image:   desiredImage,
			Template: dbosv1alpha1.WorkerVersionSetTemplate{
				Deployment:          deploymentTemplate,
				ScaledObject:        scaledObjectTemplate,
				PodDisruptionBudget: pdbTemplate,
			},
		},
	}
	for _, apply := range mutate {
		apply(set)
	}
	if err := k8sClient.Create(f.ctx, set); err != nil {
		f.t.Fatalf("create the WorkerVersionSet %s: %v", name, err)
	}
	return set
}

// createWorker writes one managed worker Deployment of a past version.
func (f *fixture) createWorker(
	set, version string,
	replicas int32,
	annotations map[string]string,
) *appsv1.Deployment {
	f.t.Helper()
	name := "dbos-worker-" + version
	labels := map[string]string{
		appNameLabel:               appName,
		ManagedByLabel:             config.DefaultManagedBy,
		config.DefaultVersionLabel: version,
		SetLabel:                   set,
	}
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   f.namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{
				appNameLabel:               appName,
				config.DefaultVersionLabel: version,
			}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
					appNameLabel:               appName,
					config.DefaultVersionLabel: version,
				}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name:  "app",
					Image: "registry.example.com/app:" + version,
				}}},
			},
		},
	}
	if err := k8sClient.Create(f.ctx, deployment); err != nil {
		f.t.Fatalf("create the worker Deployment %s: %v", name, err)
	}
	deployment.Status.Replicas = replicas
	if err := k8sClient.Status().Update(f.ctx, deployment); err != nil {
		f.t.Fatalf("set the replicas of %s: %v", name, err)
	}
	return deployment
}

// createScaledObject writes one ScaledObject for a past version, from the test template.
func (f *fixture) createScaledObject(version string) {
	f.t.Helper()
	manifest, err := render(scaledObjectTemplate, version, "unused")
	if err != nil {
		f.t.Fatalf("render the ScaledObject of %s: %v", version, err)
	}
	stamp(manifest, setName, config.DefaultManagedBy, config.DefaultVersionLabel, version)
	manifest.SetNamespace(f.namespace)
	if err := k8sClient.Create(f.ctx, manifest); err != nil {
		f.t.Fatalf("create the ScaledObject of %s: %v", version, err)
	}
}

// get reads one object of the fixture's namespace into target.
func (f *fixture) get(name string, target client.Object) error {
	return k8sClient.Get(f.ctx, types.NamespacedName{Namespace: f.namespace, Name: name}, target)
}

// deployment reads one worker Deployment, failing the test when it is gone.
func (f *fixture) deployment(name string) *appsv1.Deployment {
	f.t.Helper()
	found := &appsv1.Deployment{}
	if err := f.get(name, found); err != nil {
		f.t.Fatalf("read the Deployment %s: %v", name, err)
	}
	return found
}

// scaledObject reads one ScaledObject, failing the test when it is gone.
func (f *fixture) scaledObject(name string) *unstructured.Unstructured {
	f.t.Helper()
	found := &unstructured.Unstructured{}
	found.SetGroupVersionKind(gvkFor[kindScaledObject])
	if err := f.get(name, found); err != nil {
		f.t.Fatalf("read the ScaledObject %s: %v", name, err)
	}
	return found
}

// present reports whether one worker object of any kind is in the cluster.
func (f *fixture) present(kind workerKind, name string) bool {
	f.t.Helper()
	found := &unstructured.Unstructured{}
	found.SetGroupVersionKind(gvkFor[kind])
	return f.get(name, found) == nil
}

// patchFailer fails the first Patch of one named object of one kind, then behaves normally.
type patchFailer struct {
	client.Client
	kind  string
	name  string
	fired bool
}

func (p *patchFailer) Patch(
	ctx context.Context,
	object client.Object,
	patch client.Patch,
	options ...client.PatchOption,
) error {
	if !p.fired && object.GetName() == p.name && object.GetObjectKind().GroupVersionKind().Kind == p.kind {
		p.fired = true
		return fmt.Errorf("kube api unavailable")
	}
	return p.Client.Patch(ctx, object, patch, options...)
}

// status reads one set's status.
func (f *fixture) status(name string) dbosv1alpha1.WorkerVersionSetStatus {
	f.t.Helper()
	set := &dbosv1alpha1.WorkerVersionSet{}
	if err := f.get(name, set); err != nil {
		f.t.Fatalf("read the WorkerVersionSet %s: %v", name, err)
	}
	return set.Status
}

// ready reads one set's Ready condition.
func (f *fixture) ready(name string) metav1.Condition {
	f.t.Helper()
	for _, condition := range f.status(name).Conditions {
		if condition.Type == "Ready" {
			return condition
		}
	}
	f.t.Fatalf("the WorkerVersionSet %s has no Ready condition", name)
	return metav1.Condition{}
}

// versionStatusOf reads one entry of a set's status.versions.
func (f *fixture) versionStatusOf(set, version string) dbosv1alpha1.VersionStatus {
	f.t.Helper()
	for _, entry := range f.status(set).Versions {
		if entry.Version == version {
			return entry
		}
	}
	f.t.Fatalf("the WorkerVersionSet %s reports no version %s", set, version)
	return dbosv1alpha1.VersionStatus{}
}

// messages returns every logged message equal to want.
func (f *fixture) messages(want string) []observer.LoggedEntry {
	return f.logs.FilterMessage(want).All()
}

// workerName is the shared metadata.name of one version's three objects.
func workerName(version string) string { return "dbos-worker-" + version }
