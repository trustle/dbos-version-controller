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
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dbosv1alpha1 "github.com/trustle/dbos-version-controller/api/v1alpha1"
	"github.com/trustle/dbos-version-controller/internal/config"
)

// drainQueryFor is the query the controller writes into a draining version's KEDA trigger.
func drainQueryFor(version string) string {
	return fmt.Sprintf(
		"SELECT count(*) FROM dbos.workflow_status "+
			"WHERE application_version = '%s' AND status IN ('ENQUEUED','PENDING')",
		version,
	)
}

// soleTrigger returns the one trigger of a drained ScaledObject.
func soleTrigger(t *testing.T, object *unstructured.Unstructured) map[string]any {
	t.Helper()
	triggers, found, err := unstructured.NestedSlice(object.Object, "spec", "triggers")
	if err != nil || !found {
		t.Fatalf("the ScaledObject has no spec.triggers: %v", err)
	}
	if len(triggers) != 1 {
		t.Fatalf("expected one trigger, got %d", len(triggers))
	}
	trigger, ok := triggers[0].(map[string]any)
	if !ok {
		t.Fatalf("the trigger is not an object")
	}
	return trigger
}

func TestCreatesAllThreeObjectsForAFreshDesiredVersion(t *testing.T) {
	f := newFixture(t)
	f.createSet(setName, desiredVersion)

	f.reconcile(setName)

	name := workerName(desiredVersion)
	for _, kind := range applyOrder {
		if !f.present(kind, name) {
			t.Fatalf("the %s %s was not created", kind, name)
		}
	}
	containers := f.deployment(name).Spec.Template.Spec.Containers
	if containers[0].Image != desiredImage {
		t.Fatalf("the worker runs %q, want %q", containers[0].Image, desiredImage)
	}
}

func TestCreatedObjectsCarryTheSetLabelAndNoOwnerReference(t *testing.T) {
	f := newFixture(t)
	f.createSet(setName, desiredVersion)

	f.reconcile(setName)

	name := workerName(desiredVersion)
	for _, kind := range applyOrder {
		object := &unstructured.Unstructured{}
		object.SetGroupVersionKind(gvkFor[kind])
		if err := f.get(name, object); err != nil {
			t.Fatalf("read the %s %s: %v", kind, name, err)
		}
		labels := object.GetLabels()
		if labels[SetLabel] != setName {
			t.Fatalf("the %s carries set label %q, want %q", kind, labels[SetLabel], setName)
		}
		if labels[ManagedByLabel] != config.DefaultManagedBy {
			t.Fatalf("the %s carries managed-by %q", kind, labels[ManagedByLabel])
		}
		if labels[config.DefaultVersionLabel] != desiredVersion {
			t.Fatalf("the %s carries version label %q", kind, labels[config.DefaultVersionLabel])
		}
		if len(object.GetOwnerReferences()) != 0 {
			t.Fatalf("the %s carries an ownerReference; deletion must stay the controller's decision", kind)
		}
	}
}

func TestTheSetLabelCannotBeOverriddenByTheTemplate(t *testing.T) {
	f := newFixture(t)
	f.createSet(setName, desiredVersion, func(set *dbosv1alpha1.WorkerVersionSet) {
		set.Spec.Template.Deployment = strings.Replace(
			deploymentTemplate,
			"app.kubernetes.io/managed-by: dbos-version-controller",
			"app.kubernetes.io/managed-by: someone-else\n    "+SetLabel+": other",
			1,
		)
	})

	f.reconcile(setName)

	labels := f.deployment(workerName(desiredVersion)).Labels
	if labels[SetLabel] != setName {
		t.Fatalf("the template overrode the set label: %q", labels[SetLabel])
	}
	if labels[ManagedByLabel] != config.DefaultManagedBy {
		t.Fatalf("the template overrode the managed-by label: %q", labels[ManagedByLabel])
	}
}

func TestPatchesTheDesiredVersionWhenTheImageChanges(t *testing.T) {
	f := newFixture(t)
	set := f.createSet(setName, desiredVersion)
	f.reconcile(setName)

	if err := f.get(setName, set); err != nil {
		t.Fatalf("re-read the set: %v", err)
	}
	set.Spec.Image = "registry.example.com/app:deadbee"
	if err := k8sClient.Update(f.ctx, set); err != nil {
		t.Fatalf("update the set image: %v", err)
	}
	f.reconcile(setName)

	containers := f.deployment(workerName(desiredVersion)).Spec.Template.Spec.Containers
	if containers[0].Image != "registry.example.com/app:deadbee" {
		t.Fatalf("the worker still runs %q", containers[0].Image)
	}
}

func TestPatchingTheDesiredVersionLeavesTheReplicaCountAlone(t *testing.T) {
	f := newFixture(t)
	f.createSet(setName, desiredVersion)
	f.reconcile(setName)

	name := workerName(desiredVersion)
	scaled := f.deployment(name)
	replicas := int32(3)
	scaled.Spec.Replicas = &replicas
	if err := k8sClient.Update(f.ctx, scaled); err != nil {
		t.Fatalf("scale the worker Deployment: %v", err)
	}

	f.reconcile(setName)

	found := f.deployment(name).Spec.Replicas
	if found == nil || *found != replicas {
		t.Fatalf("the reconcile reset the replica count to %v; KEDA owns it", ptr.Deref(found, 0))
	}
}

func TestTheDeploymentPodTemplateCarriesTheVersionAndManagedByLabels(t *testing.T) {
	f := newFixture(t)
	f.createSet(setName, desiredVersion)

	f.reconcile(setName)

	deployment := f.deployment(workerName(desiredVersion))
	podLabels := deployment.Spec.Template.Labels
	if podLabels[config.DefaultVersionLabel] != desiredVersion {
		t.Fatalf("the pods carry version label %q", podLabels[config.DefaultVersionLabel])
	}
	if podLabels[ManagedByLabel] != config.DefaultManagedBy {
		t.Fatalf("the pods carry managed-by %q", podLabels[ManagedByLabel])
	}
	selector := deployment.Spec.Selector.MatchLabels
	if _, found := selector[ManagedByLabel]; found {
		t.Fatalf("stamp wrote the managed-by label into the immutable selector: %v", selector)
	}
}

func TestStampAddsThePodLabelsToATemplateThatHasNone(t *testing.T) {
	manifest, err := render(strings.Replace(deploymentTemplate, `
    metadata:
      labels:
        app.kubernetes.io/name: dbos-worker
        dbos-app-version: __VERSION__
`, "\n", 1), desiredVersion, desiredImage)
	if err != nil {
		t.Fatalf("render the Deployment: %v", err)
	}

	stamp(manifest, setName, config.DefaultManagedBy, config.DefaultVersionLabel, desiredVersion)

	podLabels, found, err := unstructured.NestedStringMap(manifest.Object, podLabelPath...)
	if err != nil || !found {
		t.Fatalf("the pod template carries no labels: %v", err)
	}
	if podLabels[config.DefaultVersionLabel] != desiredVersion || podLabels[ManagedByLabel] != config.DefaultManagedBy {
		t.Fatalf("the pod labels are %v", podLabels)
	}
}

func TestNonDesiredVersionIsParkedWithASingleOutstandingTrigger(t *testing.T) {
	f := newFixture(t)
	f.createSet(setName, desiredVersion)
	f.createWorker(setName, oldVersion, 2, nil)
	f.createScaledObject(oldVersion)
	f.rows[oldVersion] = 3

	f.reconcile(setName)

	object := f.scaledObject(workerName(oldVersion))
	minimum, _, _ := unstructured.NestedInt64(object.Object, "spec", "minReplicaCount")
	if minimum != 0 {
		t.Fatalf("minReplicaCount is %d, want 0", minimum)
	}
	trigger := soleTrigger(t, object)
	if trigger["type"] != "postgresql" {
		t.Fatalf("the trigger type is %v", trigger["type"])
	}
	if trigger["metricType"] != "AverageValue" {
		t.Fatalf("the trigger metricType is %v", trigger["metricType"])
	}
	authenticationRef, _ := trigger["authenticationRef"].(map[string]any)
	if authenticationRef["name"] != "dbos-postgres-auth" {
		t.Fatalf("the authenticationRef was lost: %v", trigger["authenticationRef"])
	}
	metadata, _ := trigger["metadata"].(map[string]any)
	if metadata["targetQueryValue"] != "1" {
		t.Fatalf("targetQueryValue is %v, want \"1\"", metadata["targetQueryValue"])
	}
	if metadata["query"] != drainQueryFor(oldVersion) {
		t.Fatalf("the drain query is %v", metadata["query"])
	}
	if !f.present(kindDeployment, workerName(oldVersion)) {
		t.Fatal("the draining Deployment was deleted while it still had work")
	}
}

func TestCreatesTheScaledObjectWhenDrainingAVersionWhoseScaledObjectIsMissing(t *testing.T) {
	f := newFixture(t)
	f.createSet(setName, desiredVersion)
	f.createWorker(setName, oldVersion, 2, nil)
	f.rows[oldVersion] = 3

	f.reconcile(setName)

	object := f.scaledObject(workerName(oldVersion))
	if object.GetLabels()[SetLabel] != setName {
		t.Fatalf("the re-created ScaledObject carries set label %q", object.GetLabels()[SetLabel])
	}
	if len(object.GetOwnerReferences()) != 0 {
		t.Fatal("the re-created ScaledObject carries an ownerReference")
	}
	minimum, _, _ := unstructured.NestedInt64(object.Object, "spec", "minReplicaCount")
	if minimum != 0 {
		t.Fatalf("minReplicaCount is %d, want 0", minimum)
	}
	target, _, _ := unstructured.NestedString(object.Object, "spec", "scaleTargetRef", "name")
	if target != workerName(oldVersion) {
		t.Fatalf("scaleTargetRef is %q", target)
	}
	metadata, _ := soleTrigger(t, object)["metadata"].(map[string]any)
	if metadata["query"] != drainQueryFor(oldVersion) {
		t.Fatalf("the drain query is %v", metadata["query"])
	}
}

func TestDesiredVersionIsNeverDrainedOrDeletedEvenAtZero(t *testing.T) {
	f := newFixture(t)
	f.createSet(setName, desiredVersion)
	f.createWorker(setName, desiredVersion, 0, nil)

	f.reconcile(setName)

	deployment := f.deployment(workerName(desiredVersion))
	if _, marked := deployment.Annotations[DrainedSinceAnnotation]; marked {
		t.Fatal("the desired version was marked drained")
	}
	minimum, found, _ := unstructured.NestedInt64(
		f.scaledObject(workerName(desiredVersion)).Object, "spec", "minReplicaCount")
	if !found || minimum != 1 {
		t.Fatalf("the desired version's ScaledObject was parked: minReplicaCount %d", minimum)
	}
}

func TestFirstIdleTickStampsTheDrainedMarkerAndDeletesNothing(t *testing.T) {
	f := newFixture(t)
	f.createSet(setName, desiredVersion)
	f.createWorker(setName, oldVersion, 0, nil)

	f.reconcile(setName)

	deployment := f.deployment(workerName(oldVersion))
	if deployment.Annotations[DrainedSinceAnnotation] != nowStamp {
		t.Fatalf("the drained marker is %q, want %q", deployment.Annotations[DrainedSinceAnnotation], nowStamp)
	}
	entry := f.versionStatusOf(setName, oldVersion)
	if entry.Phase != dbosv1alpha1.PhaseDrained {
		t.Fatalf("the phase is %q, want Drained", entry.Phase)
	}
}

func TestDeletesOnlyAfterTheGracePeriodElapses(t *testing.T) {
	inGrace := newFixture(t)
	inGrace.createSet(setName, desiredVersion)
	inGrace.createWorker(setName, oldVersion, 0, map[string]string{
		DrainedSinceAnnotation: timestamp(now.Add(-599 * time.Second)),
	})
	inGrace.createScaledObject(oldVersion)
	inGrace.reconcile(setName)

	if !inGrace.present(kindDeployment, workerName(oldVersion)) {
		t.Fatal("a version deleted one second before its grace period elapsed")
	}

	expired := newFixture(t)
	expired.createSet(setName, desiredVersion)
	expired.createWorker(setName, oldVersion, 0, map[string]string{
		DrainedSinceAnnotation: timestamp(now.Add(-601 * time.Second)),
	})
	expired.createScaledObject(oldVersion)
	expired.reconcile(setName)

	for _, kind := range deleteOrder {
		if expired.present(kind, workerName(oldVersion)) {
			t.Fatalf("the %s of the drained version survived its grace period", kind)
		}
	}
}

func TestSpecGraceSecondsOverridesTheControllerDefault(t *testing.T) {
	marker := map[string]string{DrainedSinceAnnotation: timestamp(now.Add(-90 * time.Second))}

	longer := newFixture(t)
	longer.createSet(setName, desiredVersion, func(set *dbosv1alpha1.WorkerVersionSet) {
		seconds := 120
		set.Spec.GraceSeconds = &seconds
	})
	longer.createWorker(setName, oldVersion, 0, marker)
	longer.reconcile(setName)
	if !longer.present(kindDeployment, workerName(oldVersion)) {
		t.Fatal("spec.graceSeconds 120 did not hold the version for 90 seconds of idleness")
	}

	shorter := newFixture(t)
	shorter.createSet(setName, desiredVersion, func(set *dbosv1alpha1.WorkerVersionSet) {
		seconds := 60
		set.Spec.GraceSeconds = &seconds
	})
	shorter.createWorker(setName, oldVersion, 0, marker)
	shorter.reconcile(setName)
	if shorter.present(kindDeployment, workerName(oldVersion)) {
		t.Fatal("spec.graceSeconds 60 did not release the version after 90 seconds of idleness")
	}
}

func TestClearsTheMarkerWhenWorkflowsReappear(t *testing.T) {
	f := newFixture(t)
	f.createSet(setName, desiredVersion)
	f.createWorker(setName, oldVersion, 0, map[string]string{
		DrainedSinceAnnotation: timestamp(now.Add(-2 * time.Hour)),
	})
	f.rows[oldVersion] = 7

	f.reconcile(setName)

	deployment := f.deployment(workerName(oldVersion))
	if _, marked := deployment.Annotations[DrainedSinceAnnotation]; marked {
		t.Fatal("the drained marker survived work returning to the version")
	}
	if !f.present(kindDeployment, workerName(oldVersion)) {
		t.Fatal("a busy version was deleted")
	}
}

func TestAPodOutsideTheSetThatCarriesTheVersionKeepsItPastTheGracePeriod(t *testing.T) {
	f := newFixture(t)
	f.createSet(setName, desiredVersion)
	f.createWorker(setName, oldVersion, 0, map[string]string{
		DrainedSinceAnnotation: timestamp(now.Add(-2 * time.Hour)),
	})
	f.createScaledObject(oldVersion)
	// An API pod still on the old build: it enqueues work at its own version.
	f.createPod("api-old", map[string]string{config.DefaultVersionLabel: oldVersion}, corev1.PodRunning)

	f.reconcile(setName)

	if !f.present(kindDeployment, workerName(oldVersion)) {
		t.Fatal("a version deleted while a pod outside the set still carried it")
	}
	if _, marked := f.deployment(workerName(oldVersion)).Annotations[DrainedSinceAnnotation]; marked {
		t.Fatal("the drained marker survived a pod that still carries the version")
	}
}

func TestOnlyLivePodsOutsideTheSetHoldAVersion(t *testing.T) {
	f := newFixture(t)
	f.createSet(setName, desiredVersion)
	f.createWorker(setName, oldVersion, 0, map[string]string{
		DrainedSinceAnnotation: timestamp(now.Add(-2 * time.Hour)),
	})
	f.createScaledObject(oldVersion)
	// A finished pod, and one of the controller's own worker pods, do not hold the version.
	f.createPod("api-done", map[string]string{config.DefaultVersionLabel: oldVersion}, corev1.PodSucceeded)
	f.createPod("worker-own", map[string]string{
		config.DefaultVersionLabel: oldVersion,
		ManagedByLabel:             config.DefaultManagedBy,
	}, corev1.PodRunning)

	f.reconcile(setName)

	if f.present(kindDeployment, workerName(oldVersion)) {
		t.Fatal("a finished pod or a worker pod of the set kept a drained version alive")
	}
}

func TestATerminatingPodStillHoldsItsVersion(t *testing.T) {
	f := newFixture(t)
	f.createSet(setName, desiredVersion)
	f.createWorker(setName, oldVersion, 0, map[string]string{
		DrainedSinceAnnotation: timestamp(now.Add(-2 * time.Hour)),
	})
	f.createScaledObject(oldVersion)
	f.createPod("api-stopping", map[string]string{config.DefaultVersionLabel: oldVersion}, corev1.PodRunning)
	// A finalizer keeps the pod in its graceful-termination window after the delete.
	pod := &corev1.Pod{}
	if err := f.get("api-stopping", pod); err != nil {
		t.Fatalf("read the pod: %v", err)
	}
	pod.Finalizers = []string{"test.dbos.trustle.com/hold"}
	if err := k8sClient.Update(f.ctx, pod); err != nil {
		t.Fatalf("add the finalizer: %v", err)
	}
	if err := k8sClient.Delete(f.ctx, pod); err != nil {
		t.Fatalf("delete the pod: %v", err)
	}
	t.Cleanup(func() {
		if err := f.get("api-stopping", pod); err == nil {
			pod.Finalizers = nil
			_ = k8sClient.Update(f.ctx, pod)
		}
	})

	f.reconcile(setName)

	if !f.present(kindDeployment, workerName(oldVersion)) {
		t.Fatal("a version deleted while a terminating pod still carried it")
	}
}

func TestAnUnconfiguredPodReaderDeletesNothing(t *testing.T) {
	f := newFixture(t)
	f.reconciler.Pods = nil
	f.createSet(setName, desiredVersion)
	f.createWorker(setName, oldVersion, 0, map[string]string{
		DrainedSinceAnnotation: timestamp(now.Add(-2 * time.Hour)),
	})
	f.createScaledObject(oldVersion)

	f.reconcile(setName)

	if !f.present(kindDeployment, workerName(oldVersion)) {
		t.Fatal("a version deleted with no pod reader to check who still carries it")
	}
}

func TestAPodListFailureDeletesNothing(t *testing.T) {
	f := newFixture(t)
	f.reconciler.Pods = failingReader{}
	f.createSet(setName, desiredVersion)
	f.createWorker(setName, oldVersion, 0, map[string]string{
		DrainedSinceAnnotation: timestamp(now.Add(-2 * time.Hour)),
	})
	f.createScaledObject(oldVersion)

	f.reconcile(setName)

	if !f.present(kindDeployment, workerName(oldVersion)) {
		t.Fatal("a version deleted while the pods that carry it could not be listed")
	}
}

func TestAnUnreachableDatabaseDeletesNothingAndReportsDatabaseUnavailable(t *testing.T) {
	f := newFixture(t)
	f.createSet(setName, desiredVersion)
	expired := map[string]string{DrainedSinceAnnotation: timestamp(now.Add(-2 * time.Hour))}
	for _, version := range []string{oldVersion, otherVersion} {
		f.createWorker(setName, version, 0, expired)
		f.createScaledObject(version)
	}
	f.countErr = fmt.Errorf("database unreachable")

	f.reconcile(setName)

	for _, version := range []string{oldVersion, otherVersion} {
		if !f.present(kindDeployment, workerName(version)) {
			t.Fatalf("the version %s was deleted while the database was unreachable", version)
		}
		minimum, _, _ := unstructured.NestedInt64(
			f.scaledObject(workerName(version)).Object, "spec", "minReplicaCount")
		if minimum != 0 {
			t.Fatalf("the version %s was not parked", version)
		}
		if f.deployment(workerName(version)).Annotations[DrainedSinceAnnotation] != expired[DrainedSinceAnnotation] {
			t.Fatalf("the marker of %s was rewritten on an unknown row count", version)
		}
	}

	ready := f.ready(setName)
	if ready.Reason != ReasonDatabaseUnavailable || ready.Status != "False" {
		t.Fatalf("the Ready condition is %s/%s", ready.Status, ready.Reason)
	}
	if f.versionStatusOf(setName, oldVersion).Outstanding != nil {
		t.Fatal("an unknown row count was reported as a number")
	}
}

func TestAFailedDrainDoesNotBlockTheRemainingVersions(t *testing.T) {
	f := newFixture(t)
	f.reconciler.Client = &patchFailer{Client: k8sClient, kind: "ScaledObject", name: workerName(oldVersion)}
	f.createSet(setName, desiredVersion)
	for _, version := range []string{oldVersion, otherVersion} {
		f.createWorker(setName, version, 0, nil)
		f.createScaledObject(version)
	}

	f.reconcile(setName)

	if f.deployment(workerName(otherVersion)).Annotations[DrainedSinceAnnotation] != nowStamp {
		t.Fatal("a failed drain stopped the remaining versions")
	}
	ready := f.ready(setName)
	if ready.Reason != ReasonDrainFailed {
		t.Fatalf("the Ready reason is %q, want DrainFailed", ready.Reason)
	}
	if !strings.Contains(ready.Message, oldVersion) {
		t.Fatalf("the message does not name the failed version: %q", ready.Message)
	}
	if strings.Contains(ready.Message, otherVersion) {
		t.Fatalf("the message names a version that drained: %q", ready.Message)
	}
}

func TestDatabaseUnavailableTakesPrecedenceOverAFailedDrain(t *testing.T) {
	f := newFixture(t)
	f.reconciler.Client = &patchFailer{Client: k8sClient, kind: "ScaledObject", name: workerName(oldVersion)}
	f.createSet(setName, desiredVersion)
	f.createWorker(setName, oldVersion, 0, nil)
	f.createScaledObject(oldVersion)
	f.countErr = fmt.Errorf("database unreachable")

	f.reconcile(setName)

	if reason := f.ready(setName).Reason; reason != ReasonDatabaseUnavailable {
		t.Fatalf("the Ready reason is %q, want DatabaseUnavailable", reason)
	}
}

func TestAnInvalidSpecWritesNothingAndReportsInvalidSpec(t *testing.T) {
	f := newFixture(t)
	f.createSet(setName, desiredVersion, func(set *dbosv1alpha1.WorkerVersionSet) {
		set.Spec.Template.Deployment = ""
	})
	expired := map[string]string{DrainedSinceAnnotation: timestamp(now.Add(-2 * time.Hour))}
	worker := f.createWorker(setName, oldVersion, 0, expired)

	f.reconcile(setName)

	if f.present(kindDeployment, workerName(desiredVersion)) {
		t.Fatal("an invalid spec still created the desired version")
	}
	if !f.present(kindDeployment, workerName(oldVersion)) {
		t.Fatal("an invalid spec still deleted a drained version")
	}
	if after := f.deployment(workerName(oldVersion)); after.ResourceVersion != worker.ResourceVersion {
		t.Fatal("an invalid spec still wrote to a worker Deployment")
	}
	ready := f.ready(setName)
	if ready.Status != "False" || ready.Reason != ReasonInvalidSpec {
		t.Fatalf("the Ready condition is %s/%s", ready.Status, ready.Reason)
	}
	if len(f.status(setName).Versions) != 0 {
		t.Fatal("an invalid spec reported versions it never read")
	}
}

func TestSpecProblemRejectsAnUnusableSpec(t *testing.T) {
	usable := dbosv1alpha1.WorkerVersionSetSpec{
		Version: desiredVersion,
		Image:   desiredImage,
		Template: dbosv1alpha1.WorkerVersionSetTemplate{
			Deployment:          deploymentTemplate,
			ScaledObject:        scaledObjectTemplate,
			PodDisruptionBudget: pdbTemplate,
		},
	}
	if problem := specProblem(usable); problem != "" {
		t.Fatalf("a usable spec was rejected: %s", problem)
	}

	cases := map[string]func(*dbosv1alpha1.WorkerVersionSetSpec){
		"an upper-case version": func(s *dbosv1alpha1.WorkerVersionSetSpec) { s.Version = "V20260916135248" },
		"an empty version":      func(s *dbosv1alpha1.WorkerVersionSetSpec) { s.Version = "" },
		"a quoted version":      func(s *dbosv1alpha1.WorkerVersionSetSpec) { s.Version = "v1'; drop table x--" },
		"a shell image":         func(s *dbosv1alpha1.WorkerVersionSetSpec) { s.Image = "app:latest; rm -rf /" },
		"a missing template":    func(s *dbosv1alpha1.WorkerVersionSetSpec) { s.Template.ScaledObject = "" },
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			spec := usable
			breakIt(&spec)
			if problem := specProblem(spec); problem == "" {
				t.Fatalf("%s was accepted", name)
			}
		})
	}
}

func TestTwoSetsInOneNamespaceReconcileWithoutDrainingEachOther(t *testing.T) {
	const otherSet = "other-worker"
	f := newFixture(t)
	f.createSet(setName, desiredVersion)
	f.createSet(otherSet, otherVersion)
	f.createWorker(setName, oldVersion, 0, nil)
	f.createWorker(otherSet, otherVersion, 0, nil)

	f.reconcile(setName)
	f.reconcile(otherSet)

	if f.deployment(workerName(oldVersion)).Annotations[DrainedSinceAnnotation] != nowStamp {
		t.Fatal("the set's own old version was not marked drained")
	}
	if _, marked := f.deployment(workerName(otherVersion)).Annotations[DrainedSinceAnnotation]; marked {
		t.Fatal("one set drained the other set's desired version")
	}

	first := f.status(setName).Versions
	if len(first) != 2 || first[0].Version != desiredVersion || first[1].Version != oldVersion {
		t.Fatalf("the first set reports %v", first)
	}
	second := f.status(otherSet).Versions
	if len(second) != 1 || second[0].Version != otherVersion {
		t.Fatalf("the second set reports %v", second)
	}
	if phase := f.versionStatusOf(otherSet, otherVersion).Phase; phase != dbosv1alpha1.PhaseCurrent {
		t.Fatalf("the second set reports its own version as %q", phase)
	}
	if reason := f.ready(otherSet).Reason; reason != ReasonReconciled {
		t.Fatalf("the second set is %q", reason)
	}
}

func TestStatusReportsEachPhaseAndKeepsDrainedSinceToDrainedVersions(t *testing.T) {
	f := newFixture(t)
	f.createSet(setName, desiredVersion)
	f.reconcile(setName)

	current := f.deployment(workerName(desiredVersion))
	current.Status.Replicas = 3
	if err := k8sClient.Status().Update(f.ctx, current); err != nil {
		t.Fatalf("set the replicas of the desired version: %v", err)
	}
	f.createWorker(setName, otherVersion, 2, nil)
	f.createWorker(setName, oldVersion, 0, nil)
	f.rows[otherVersion] = 4

	f.reconcile(setName)

	status := f.status(setName)
	if status.ObservedGeneration != 1 {
		t.Fatalf("observedGeneration is %d", status.ObservedGeneration)
	}
	if status.LastReconcileTime == nil || status.LastReconcileTime.UTC() != now {
		t.Fatalf("lastReconcileTime is %v", status.LastReconcileTime)
	}

	desired := f.versionStatusOf(setName, desiredVersion)
	if desired.Phase != dbosv1alpha1.PhaseCurrent || desired.Replicas != 3 || desired.Image != desiredImage {
		t.Fatalf("the desired version reports %+v", desired)
	}
	if desired.Outstanding == nil || *desired.Outstanding != 0 {
		t.Fatal("outstanding is not reported for the current version")
	}
	if desired.DrainedSince != nil {
		t.Fatal("the current version carries drainedSince")
	}

	busy := f.versionStatusOf(setName, otherVersion)
	if busy.Phase != dbosv1alpha1.PhaseDraining || busy.Replicas != 2 {
		t.Fatalf("the busy version reports %+v", busy)
	}
	if busy.Outstanding == nil || *busy.Outstanding != 4 {
		t.Fatalf("the busy version reports outstanding %v", busy.Outstanding)
	}
	if busy.DrainedSince != nil {
		t.Fatal("a draining version carries drainedSince")
	}

	idle := f.versionStatusOf(setName, oldVersion)
	if idle.Phase != dbosv1alpha1.PhaseDrained || idle.Replicas != 0 {
		t.Fatalf("the idle version reports %+v", idle)
	}
	if idle.DrainedSince == nil || idle.DrainedSince.UTC() != now {
		t.Fatalf("the idle version reports drainedSince %v", idle.DrainedSince)
	}
	if reason := f.ready(setName).Reason; reason != ReasonReconciled {
		t.Fatalf("the Ready reason is %q", reason)
	}
}

func TestTheReadyTransitionTimeSurvivesATickThatChangesNothing(t *testing.T) {
	f := newFixture(t)
	set := f.createSet(setName, desiredVersion)
	f.reconcile(setName)

	first := f.ready(setName).LastTransitionTime
	if first.UTC() != now {
		t.Fatalf("the first transition time is %v", first)
	}

	f.clock = now.Add(time.Hour)
	f.reconcile(setName)
	if again := f.ready(setName).LastTransitionTime; !again.Equal(&first) {
		t.Fatalf("an unchanged Ready condition moved its transition time to %v", again)
	}

	if err := f.get(setName, set); err != nil {
		t.Fatalf("re-read the set: %v", err)
	}
	set.Spec.Template.Deployment = ""
	if err := k8sClient.Update(f.ctx, set); err != nil {
		t.Fatalf("break the set spec: %v", err)
	}
	f.reconcile(setName)

	flipped := f.ready(setName)
	if flipped.Reason != ReasonInvalidSpec {
		t.Fatalf("the Ready reason is %q", flipped.Reason)
	}
	if flipped.LastTransitionTime.UTC() != now.Add(time.Hour) {
		t.Fatalf("a changed Ready condition kept its transition time: %v", flipped.LastTransitionTime)
	}
}

func TestSteadyStateWritesNoMarkerAndNoWorkerPatch(t *testing.T) {
	f := newFixture(t)
	f.createSet(setName, desiredVersion)
	f.reconcile(setName)

	before := f.deployment(workerName(desiredVersion)).ResourceVersion
	f.reconcile(setName)
	after := f.deployment(workerName(desiredVersion))

	if after.ResourceVersion != before {
		t.Fatalf("a steady-state tick rewrote the desired Deployment: %s to %s", before, after.ResourceVersion)
	}
	if _, marked := after.Annotations[DrainedSinceAnnotation]; marked {
		t.Fatal("a steady-state tick wrote a drained marker")
	}
}

func TestDryRunPerformsNoWriteButRecordsWhatItObserved(t *testing.T) {
	f := newFixture(t, dryRun)
	f.createSet(setName, desiredVersion)
	f.createWorker(setName, oldVersion, 0, nil)

	f.reconcile(setName)

	for _, kind := range applyOrder {
		if f.present(kind, workerName(desiredVersion)) {
			t.Fatalf("dry run created the %s of the desired version", kind)
		}
	}
	if f.deployment(workerName(oldVersion)).Annotations[DrainedSinceAnnotation] != nowStamp {
		t.Fatal("dry run did not write the drained marker, so its grace clock never advances")
	}
	if reason := f.ready(setName).Reason; reason != ReasonReconciled {
		t.Fatalf("dry run did not write the status: %q", reason)
	}
	if f.present(kindScaledObject, workerName(oldVersion)) {
		t.Fatal("dry run created the draining version's missing ScaledObject")
	}
	// The three objects of the desired version, plus the draining version's missing
	// ScaledObject, are all previewed rather than written.
	if previews := f.messages("Would create worker object"); len(previews) != 4 {
		t.Fatalf("dry run logged %d create previews, want 4", len(previews))
	}
}

func TestDryRunPastGracePreviewsAllThreeDeletes(t *testing.T) {
	f := newFixture(t, dryRun)
	f.createSet(setName, desiredVersion)
	f.createWorker(setName, oldVersion, 0, map[string]string{
		DrainedSinceAnnotation: timestamp(now.Add(-601 * time.Second)),
	})
	f.createScaledObject(oldVersion)

	f.reconcile(setName)

	if previews := f.messages("Would delete worker object"); len(previews) != 3 {
		t.Fatalf("dry run logged %d delete previews, want 3", len(previews))
	}
	if !f.present(kindDeployment, workerName(oldVersion)) {
		t.Fatal("dry run deleted a worker Deployment")
	}
	if !f.present(kindScaledObject, workerName(oldVersion)) {
		t.Fatal("dry run deleted a ScaledObject")
	}
}

func TestRollbackGetsAFreshGracePeriodInsteadOfAnImmediateDelete(t *testing.T) {
	assertRollbackRestartsTheClock(t, newFixture(t))
}

func TestRollbackGetsAFreshGracePeriodUnderDryRunToo(t *testing.T) {
	assertRollbackRestartsTheClock(t, newFixture(t, dryRun))
}

// assertRollbackRestartsTheClock rolls back onto a stale drained version, then rolls forward
// again, and checks the version served a fresh grace period rather than an immediate delete.
//
// The marker clear goes through mark, which bypasses dry run like drain's own marker writes.
// Without that, a rollback previewed under dry run leaves the stale marker in place, and the next
// drain tick sees a marker already past grace and previews an immediate delete.
func assertRollbackRestartsTheClock(t *testing.T, f *fixture) {
	t.Helper()
	set := f.createSet(setName, oldVersion)
	f.createWorker(setName, oldVersion, 0, map[string]string{
		DrainedSinceAnnotation: timestamp(now.Add(-2 * time.Hour)),
	})
	f.createScaledObject(oldVersion)

	f.reconcile(setName) // oldVersion is desired again: its stale marker is cleared.

	if _, marked := f.deployment(workerName(oldVersion)).Annotations[DrainedSinceAnnotation]; marked {
		t.Fatal("the rollback left a stale drained marker in place")
	}

	if err := f.get(setName, set); err != nil {
		t.Fatalf("re-read the set: %v", err)
	}
	set.Spec.Version = desiredVersion
	if err := k8sClient.Update(f.ctx, set); err != nil {
		t.Fatalf("roll forward again: %v", err)
	}

	f.reconcile(setName) // oldVersion drains again and hits zero rows and zero pods.

	if marker := f.deployment(workerName(oldVersion)).Annotations[DrainedSinceAnnotation]; marker != nowStamp {
		t.Fatalf("the drained marker is %q, want a fresh %q", marker, nowStamp)
	}
	if !f.present(kindDeployment, workerName(oldVersion)) {
		t.Fatal("the rolled-back version was deleted without a fresh grace period")
	}
	if previews := f.messages("Would delete worker object"); len(previews) != 0 {
		t.Fatalf("dry run previewed %d deletes of a version with a fresh grace period", len(previews))
	}
}

// failingReader fails every read, standing in for a pod list the API server refuses.
type failingReader struct{}

func (failingReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return errors.New("pods are forbidden")
}

func (failingReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return errors.New("pods are forbidden")
}
