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

// Package e2e runs the controller's own image in a kind cluster, against a real Postgres.
//
// The envtest suite in internal/controller covers the reconcile logic against an API server, but
// it stubs the workflow counter and it never builds the image. This suite closes both gaps: it
// deploys the image the Dockerfile produces, points it at a Postgres holding dbos.workflow_status,
// and drives one WorkerVersionSet through a version change and a deletion.
//
// It installs the KEDA ScaledObject CRD and not KEDA itself. The controller creates and patches
// ScaledObjects; it never reads what KEDA does with them, and nothing in the reconcile loop waits
// for a KEDA decision. Where the suite needs the effect of KEDA scaling a parked version to zero,
// it scales the Deployment itself. Running KEDA would add a scaler, a TriggerAuthentication and a
// second reconcile loop without testing another line of this controller.
package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dbosv1alpha1 "github.com/trustle/dbos-version-controller/api/v1alpha1"
)

const (
	// workerNamespace holds the WorkerVersionSet, the worker objects and the Postgres.
	workerNamespace = "dbos-workers"
	// controllerNamespace and controllerDeployment are what config/default renders.
	controllerNamespace  = "dbos-version-controller-system"
	controllerDeployment = "dbos-version-controller-controller-manager"

	// image is built from the repository's Dockerfile and loaded into the kind node.
	image = "dbos-version-controller:e2e"

	setName       = "e2e-worker"
	firstVersion  = "v1"
	secondVersion = "v2"

	// interval is short, so a spec waits seconds rather than the default half minute.
	interval = "5"

	// timeout bounds a single Eventually. The slowest one waits for an image pull.
	timeout = 3 * time.Minute
	poll    = 2 * time.Second
	// holdWindow is how long a version holding work must survive. It is longer than the set's
	// graceSeconds, so a version that was going to be deleted would be gone inside it.
	holdWindow = 30 * time.Second
)

// repoRoot is where every shelled-out command runs. The test binary runs in test/e2e.
const repoRoot = "../.."

var (
	scheme   = runtime.NewScheme()
	k8s      client.Client
	kindPath = envOr("KIND", "kind")
	cluster  = envOr("KIND_CLUSTER", "dbos-version-controller-test-e2e")
)

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "dbos-version-controller e2e suite")
}

var _ = BeforeSuite(func() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(dbosv1alpha1.AddToScheme(scheme))

	By("building the controller image and loading it into the kind node")
	mustRun("make", "docker-build", "IMG="+image)
	mustRun(kindPath, "load", "docker-image", image, "--name", cluster)

	By("installing the CRDs")
	mustRun("make", "install")
	// The controller writes ScaledObjects, so their definition must exist. This is the same
	// KEDA 2.17.2 document the envtest suite installs.
	mustRun("kubectl", "apply", "-f", "test/crds/keda.sh_scaledobjects.yaml")

	By("starting the DBOS system database")
	applyCreated("namespace", workerNamespace)
	mustRun("kubectl", "apply", "-f", "test/e2e/testdata/dbos-database.yaml")
	mustRun("kubectl", "rollout", "status", "-n", workerNamespace,
		"deployment/dbos-database", "--timeout=3m")

	By("deploying the controller")
	applyCreated("namespace", controllerNamespace)
	applyCreated("secret", "generic", "dbos-database",
		"-n", controllerNamespace,
		"--from-literal=system-database-url="+
			"postgresql://dbos:dbos@dbos-database."+workerNamespace+".svc:5432/dbos?sslmode=disable")
	// make deploy writes the image into config/manager/kustomization.yaml, which is tracked. Put
	// it back when the suite ends, so a local run leaves the working tree as it found it.
	preserve("config/manager/kustomization.yaml")
	mustRun("make", "deploy", "IMG="+image)
	// config/default keeps the production defaults. A half-minute reconcile interval would make
	// every spec wait for it.
	mustRun("kubectl", "set", "env", "-n", controllerNamespace,
		"deployment/"+controllerDeployment, "DVC_INTERVAL_SECONDS="+interval)
	mustRun("kubectl", "rollout", "status", "-n", controllerNamespace,
		"deployment/"+controllerDeployment, "--timeout=3m")

	var err error
	k8s, err = client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: scheme})
	Expect(err).NotTo(HaveOccurred())
})

// ReportAfterSuite prints the controller's logs when a spec failed. The cluster is usually deleted
// straight afterwards, so the logs have to be collected here or not at all.
var _ = ReportAfterSuite("controller logs", func(report Report) {
	if report.SuiteSucceeded {
		return
	}
	out, err := run("kubectl", "logs", "-n", controllerNamespace,
		"deployment/"+controllerDeployment, "--tail=200")
	if err != nil {
		AddReportEntry("controller logs unavailable", err.Error())
		return
	}
	AddReportEntry("controller logs", out)
})

// preserve restores one tracked file when the suite ends.
func preserve(path string) {
	GinkgoHelper()
	full := filepath.Join(repoRoot, path)
	original, err := os.ReadFile(full)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() {
		Expect(os.WriteFile(full, original, 0o644)).To(Succeed())
	})
}

// applyCreated runs one `kubectl create` client-side and applies the result, so the step is
// idempotent. A reused cluster — one left behind by an interrupted run — would otherwise fail the
// suite with AlreadyExists on the first namespace.
func applyCreated(args ...string) {
	GinkgoHelper()
	manifest := mustRun("kubectl", append(append([]string{"create"}, args...), "--dry-run=client", "-o", "yaml")...)

	command := exec.Command("kubectl", "apply", "-f", "-")
	command.Dir = repoRoot
	command.Stdin = strings.NewReader(manifest)
	out, err := command.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "kubectl apply: %s", out)
}

// psql runs one statement against the DBOS system database.
func psql(statement string) {
	GinkgoHelper()
	mustRun("kubectl", "exec", "-n", workerNamespace, "deployment/dbos-database", "--",
		"psql", "-U", "dbos", "-d", "dbos", "-tAc", statement)
}

// run executes one command from the repository root and returns its combined output.
func run(name string, args ...string) (string, error) {
	command := exec.Command(name, args...)
	command.Dir = repoRoot
	out, err := command.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out), nil
}

// mustRun executes one command and fails the spec when it does not succeed.
func mustRun(name string, args ...string) string {
	GinkgoHelper()
	out, err := run(name, args...)
	Expect(err).NotTo(HaveOccurred())
	return out
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
