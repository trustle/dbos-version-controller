/*
Copyright 2026 Trustle, Inc.

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

package e2e

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dbosv1alpha1 "github.com/trustle/dbos-version-controller/api/v1alpha1"
)

var scaledObjectGVK = schema.GroupVersionKind{
	Group:   "keda.sh",
	Version: "v1alpha1",
	Kind:    "ScaledObject",
}

// The specs run in order: each one leaves the cluster in the state the next one starts from.
var _ = Describe("WorkerVersionSet", Ordered, func() {
	ctx := context.Background()

	It("applies the desired version's objects and reports Ready", func() {
		mustRun("kubectl", "apply", "-f", "test/e2e/testdata/workerversionset.yaml")

		Eventually(func(g Gomega) {
			g.Expect(k8s.Get(ctx, workerKey(firstVersion), &appsv1.Deployment{})).To(Succeed())
			g.Expect(k8s.Get(ctx, workerKey(firstVersion), scaledObject())).To(Succeed())
			g.Expect(k8s.Get(ctx, workerKey(firstVersion), &policyv1.PodDisruptionBudget{})).To(Succeed())
		}).WithTimeout(timeout).WithPolling(poll).Should(Succeed())

		By("reading the version back off the status subresource")
		Eventually(func(g Gomega) {
			status := readStatus(ctx, g)

			ready := meta.FindStatusCondition(status.Conditions, "Ready")
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(ready.Reason).To(Equal("Reconciled"))

			current := firstVersionEntry(g, status)
			g.Expect(current.Phase).To(Equal(dbosv1alpha1.PhaseCurrent))
			g.Expect(current.Image).To(Equal("registry.k8s.io/pause:3.10"))
			// The count comes from the controller's real query against the real Postgres, so a
			// present zero proves the database is reachable.
			g.Expect(current.Outstanding).NotTo(BeNil())
			g.Expect(*current.Outstanding).To(BeZero())
		}).WithTimeout(timeout).WithPolling(poll).Should(Succeed())
	})

	It("parks an old version and holds it on its running pod", func() {
		By("moving the set to the next version")
		// No outstanding row exists yet, so the pod the old version still runs is the only gate
		// holding it. That is what this spec proves.
		mustRun("kubectl", "patch", "workerversionset", setName, "-n", workerNamespace,
			"--type=merge", "-p", fmt.Sprintf(`{"spec":{"version":%q}}`, secondVersion))

		Eventually(func(g Gomega) {
			g.Expect(k8s.Get(ctx, workerKey(secondVersion), &appsv1.Deployment{})).To(Succeed())
		}).WithTimeout(timeout).WithPolling(poll).Should(Succeed())

		By("parking the old version's ScaledObject at zero, on its own query")
		Eventually(func(g Gomega) {
			parked := scaledObject()
			g.Expect(k8s.Get(ctx, workerKey(firstVersion), parked)).To(Succeed())

			minReplicas, found, err := unstructured.NestedInt64(parked.Object, "spec", "minReplicaCount")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(minReplicas).To(BeZero())

			triggers, found, err := unstructured.NestedSlice(parked.Object, "spec", "triggers")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(triggers).To(HaveLen(1))

			trigger, ok := triggers[0].(map[string]any)
			g.Expect(ok).To(BeTrue())
			g.Expect(trigger["type"]).To(Equal("postgresql"))
			metadata, ok := trigger["metadata"].(map[string]any)
			g.Expect(ok).To(BeTrue())
			g.Expect(metadata["query"]).To(ContainSubstring("application_version = '" + firstVersion + "'"))
		}).WithTimeout(timeout).WithPolling(poll).Should(Succeed())

		By("reporting the old version as Draining with nothing outstanding")
		Eventually(func(g Gomega) {
			old := firstVersionEntry(g, readStatus(ctx, g))
			g.Expect(old.Phase).To(Equal(dbosv1alpha1.PhaseDraining))
			g.Expect(old.Outstanding).NotTo(BeNil())
			g.Expect(*old.Outstanding).To(BeZero())
		}).WithTimeout(timeout).WithPolling(poll).Should(Succeed())

		By("keeping the old version's objects while its pod is up")
		// graceSeconds is 10, so a version held by nothing would be gone well inside this window.
		// The row count is zero: the pod is what holds it.
		Consistently(func(g Gomega) {
			g.Expect(k8s.Get(ctx, workerKey(firstVersion), &appsv1.Deployment{})).To(Succeed())
		}).WithTimeout(holdWindow).WithPolling(poll).Should(Succeed())
	})

	It("holds a parked version that still has work, with its pods at zero", func() {
		By("giving the old version one unfinished workflow")
		psql(fmt.Sprintf(
			"INSERT INTO dbos.workflow_status (workflow_uuid, application_version, status) "+
				"VALUES ('wf-1', '%s', 'PENDING')", firstVersion))

		By("scaling the parked version to zero, as KEDA would")
		// KEDA is not installed. The controller never scales a Deployment itself: it writes
		// minReplicaCount 0 and lets KEDA act. This is that act. It happens after the row exists,
		// so the two gates swap over rather than both clearing at once.
		mustRun("kubectl", "scale", "-n", workerNamespace,
			"deployment/"+workerName(firstVersion), "--replicas=0")
		Eventually(func(g Gomega) {
			deployment := &appsv1.Deployment{}
			g.Expect(k8s.Get(ctx, workerKey(firstVersion), deployment)).To(Succeed())
			g.Expect(deployment.Status.Replicas).To(BeZero())
		}).WithTimeout(timeout).WithPolling(poll).Should(Succeed())

		By("reporting the old version as Draining with its one row")
		Eventually(func(g Gomega) {
			old := firstVersionEntry(g, readStatus(ctx, g))
			g.Expect(old.Phase).To(Equal(dbosv1alpha1.PhaseDraining))
			g.Expect(old.Outstanding).NotTo(BeNil())
			g.Expect(*old.Outstanding).To(Equal(int64(1)))
		}).WithTimeout(timeout).WithPolling(poll).Should(Succeed())

		By("keeping the old version's objects while the row is there")
		// The pods are already at zero: the row is the only thing left holding it.
		Consistently(func(g Gomega) {
			g.Expect(k8s.Get(ctx, workerKey(firstVersion), &appsv1.Deployment{})).To(Succeed())
		}).WithTimeout(holdWindow).WithPolling(poll).Should(Succeed())

		By("keeping the old version while its only row is DELAYED")
		// A workflow enqueued with a delay waits as DELAYED, then becomes ENQUEUED at its own
		// version. The version must still be there when it does.
		psql("UPDATE dbos.workflow_status SET status = 'DELAYED' WHERE workflow_uuid = 'wf-1'")
		Consistently(func(g Gomega) {
			g.Expect(k8s.Get(ctx, workerKey(firstVersion), &appsv1.Deployment{})).To(Succeed())
			old := firstVersionEntry(g, readStatus(ctx, g))
			g.Expect(old.Phase).To(Equal(dbosv1alpha1.PhaseDraining))
			g.Expect(old.Outstanding).NotTo(BeNil())
			g.Expect(*old.Outstanding).To(Equal(int64(1)))
		}).WithTimeout(holdWindow).WithPolling(poll).Should(Succeed())
	})

	It("deletes the old version once it is empty, and not before the grace period", func() {
		By("finishing the last workflow")
		// Both other gates are already clear, so this starts the grace clock. The controller
		// cannot have marked the version drained before now: the marker needs zero rows.
		psql("DELETE FROM dbos.workflow_status WHERE application_version = '" + firstVersion + "'")

		By("holding the old version until the grace period is up")
		Consistently(func(g Gomega) {
			g.Expect(k8s.Get(ctx, workerKey(firstVersion), &appsv1.Deployment{})).To(Succeed())
		}).WithTimeout(graceProbe).WithPolling(poll).Should(Succeed())

		By("deleting all three objects after the grace period")
		Eventually(func(g Gomega) {
			expectGone(g, ctx, &appsv1.Deployment{})
			expectGone(g, ctx, scaledObject())
			expectGone(g, ctx, &policyv1.PodDisruptionBudget{})
		}).WithTimeout(timeout).WithPolling(poll).Should(Succeed())

		By("leaving the desired version alone")
		Expect(k8s.Get(ctx, workerKey(secondVersion), &appsv1.Deployment{})).To(Succeed())
	})
})

// expectGone asserts the first version's object of this kind is absent.
func expectGone(g Gomega, ctx context.Context, object client.Object) {
	err := k8s.Get(ctx, workerKey(firstVersion), object)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "expected %T to be deleted, got %v", object, err)
}

// readStatus returns the set's current status.
func readStatus(ctx context.Context, g Gomega) dbosv1alpha1.WorkerVersionSetStatus {
	set := &dbosv1alpha1.WorkerVersionSet{}
	g.Expect(k8s.Get(ctx, client.ObjectKey{Namespace: workerNamespace, Name: setName}, set)).To(Succeed())
	return set.Status
}

// firstVersionEntry returns the status entry of the first version, failing when it is absent.
func firstVersionEntry(g Gomega, status dbosv1alpha1.WorkerVersionSetStatus) dbosv1alpha1.VersionStatus {
	for _, entry := range status.Versions {
		if entry.Version == firstVersion {
			return entry
		}
	}
	g.Expect(status.Versions).To(ContainElement(HaveField("Version", firstVersion)))
	return dbosv1alpha1.VersionStatus{}
}

func workerName(version string) string {
	return setName + "-" + version
}

func workerKey(version string) client.ObjectKey {
	return client.ObjectKey{Namespace: workerNamespace, Name: workerName(version)}
}

func scaledObject() *unstructured.Unstructured {
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(scaledObjectGVK)
	return object
}
