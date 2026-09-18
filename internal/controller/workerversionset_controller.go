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

// Package controller reconciles the per-version worker objects of every WorkerVersionSet.
//
// A DBOS application runs one worker Deployment per application version, so a version that is
// being replaced can still finish its in-flight workflows: DBOS recovers a workflow only onto an
// executor reporting the same application version. A Git-rendered chart cannot hold that list of
// versions, because the list changes with the workload, not with the commit. Here Git holds one
// WorkerVersionSet custom resource per application and this controller owns the per-version
// objects: it applies the desired version's Deployment, ScaledObject and PDB, puts every other
// version of that set into draining, and deletes a drained version once it has held zero
// outstanding workflows and zero pods for the grace period. It reports what it saw on the set's
// status.
//
// The controller never launches DBOS. It reads dbos.workflow_status over a plain SQL connection,
// which keeps a crashing worker version from taking the controller with it.
package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	dbosv1alpha1 "github.com/trustle/dbos-version-controller/api/v1alpha1"
	"github.com/trustle/dbos-version-controller/internal/config"
)

// OutstandingFunc counts the unfinished workflows of one DBOS application version.
type OutstandingFunc func(ctx context.Context, version string) (int64, error)

// WorkerVersionSetReconciler reconciles a WorkerVersionSet object.
type WorkerVersionSetReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Config config.Config

	// Outstanding counts the unfinished workflows of one application version.
	Outstanding OutstandingFunc
	// Clock reads the current time. Tests replace it.
	Clock func() time.Time
}

// managedDeployment is one worker Deployment this controller owns.
type managedDeployment struct {
	Name        string
	Version     string
	Replicas    int32
	Annotations map[string]string
	Image       string
}

// +kubebuilder:rbac:groups=dbos.trustle.com,resources=workerversionsets,verbs=get;list;watch
// +kubebuilder:rbac:groups=dbos.trustle.com,resources=workerversionsets/status,verbs=get;patch;update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;patch;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;create;patch;delete
// +kubebuilder:rbac:groups=keda.sh,resources=scaledobjects,verbs=get;create;patch;delete

// Reconcile brings one WorkerVersionSet's worker objects to their desired state and patches its
// status. A set that fails is reported on its own status and reconciled again next interval.
func (r *WorkerVersionSetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	set := &dbosv1alpha1.WorkerVersionSet{}
	if err := r.Get(ctx, req.NamespacedName, set); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	now := r.now()
	if err := r.reconcileSet(ctx, set, now); err != nil {
		logger.Error(err, "Failed to reconcile WorkerVersionSet", "name", set.Name)
		r.report(ctx, set, now, nil, metav1.ConditionFalse, ReasonReconcileError, err.Error())
	}

	return ctrl.Result{RequeueAfter: r.Config.Interval}, nil
}

// reconcileSet reconciles one WorkerVersionSet and patches its status.
//
// An unusable spec skips every write for this set: the controller deletes nothing while it cannot
// tell which version is desired.
func (r *WorkerVersionSetReconciler) reconcileSet(
	ctx context.Context,
	set *dbosv1alpha1.WorkerVersionSet,
	now time.Time,
) error {
	logger := log.FromContext(ctx)

	if problem := specProblem(set.Spec); problem != "" {
		logger.Info("WorkerVersionSet spec is unusable; skipping it", "name", set.Name, "problem", problem)
		r.report(ctx, set, now, nil, metav1.ConditionFalse, ReasonInvalidSpec, problem)
		return nil
	}

	version := strings.TrimSpace(set.Spec.Version)
	image := strings.TrimSpace(set.Spec.Image)

	grace := r.Config.Grace
	if set.Spec.GraceSeconds != nil {
		grace = time.Duration(*set.Spec.GraceSeconds) * time.Second
	}

	managed, err := r.listManaged(ctx, set.Name)
	if err != nil {
		return fmt.Errorf("list managed deployments: %w", err)
	}
	var existing *managedDeployment
	for index := range managed {
		if managed[index].Version == version {
			existing = &managed[index]
			break
		}
	}

	templateScaledObject, err := r.applyDesired(ctx, set, version, image, existing)
	if err != nil {
		return err
	}

	// An unknown row count never deletes: a version with work we cannot see must keep its
	// objects. The first database failure latches for the rest of this reconcile.
	databaseAvailable := true
	count := func(target string) *int64 {
		if !databaseAvailable {
			return nil
		}
		rows, err := r.Outstanding(ctx, target)
		if err != nil {
			databaseAvailable = false
			logger.Error(err, "Cannot count outstanding workflows; deleting nothing this tick")
			return nil
		}
		return &rows
	}

	currentRows := count(version)
	desired := versionStatus(version, dbosv1alpha1.PhaseCurrent, image, 0, currentRows)
	var draining []dbosv1alpha1.VersionStatus
	var failedDrains []string

	for index := range managed {
		deployment := managed[index]
		if deployment.Version == version {
			deployedImage := deployment.Image
			if deployedImage == "" {
				deployedImage = image
			}
			desired = versionStatus(version, dbosv1alpha1.PhaseCurrent, deployedImage, deployment.Replicas, currentRows)
			continue
		}
		if !versionPattern.MatchString(deployment.Version) {
			logger.Info("Managed worker carries an unusable version label; leaving it alone", "name", deployment.Name)
			continue
		}

		rows := count(deployment.Version)
		entry, err := r.drain(ctx, set, deployment, templateScaledObject, rows, now, grace)
		if err != nil {
			logger.Error(err, "Failed to drain worker version; continuing with the remaining versions",
				"name", deployment.Name, "version", deployment.Version)
			failedDrains = append(failedDrains, deployment.Version)
		}
		draining = append(draining, entry)
	}

	versions := append([]dbosv1alpha1.VersionStatus{desired}, draining...)

	// DatabaseUnavailable takes precedence over DrainFailed: an unreachable database explains
	// every drain failure that follows from it, and is the more actionable cause.
	switch {
	case !databaseAvailable:
		r.report(ctx, set, now, versions, metav1.ConditionFalse, ReasonDatabaseUnavailable,
			"Cannot count outstanding workflows; deleted nothing this tick")
	case len(failedDrains) > 0:
		r.report(ctx, set, now, versions, metav1.ConditionFalse, ReasonDrainFailed,
			fmt.Sprintf("Failed to drain version(s): %s", strings.Join(failedDrains, ", ")))
	default:
		r.report(ctx, set, now, versions, metav1.ConditionTrue, ReasonReconciled, "Reconciled the worker versions")
	}

	return nil
}

// applyDesired creates or patches the three objects of the desired version, and returns the
// rendered ScaledObject that seeds every drain trigger.
//
// Clearing the drained-since marker on an existing Deployment goes through mark, not patch: mark
// bypasses dry run the same way drain writes the marker, so a rollback in dry run does not leave
// a stale marker that the next drain would read as already past grace.
//
// mark is called only when the existing Deployment still carries the marker: at steady state the
// desired version never drained, so there is nothing to clear and calling mark every tick would
// be a no-op write (and a dry-run write when nothing was observed).
//
// An existing Deployment is patched without spec.replicas, because KEDA owns the replica count of
// a live version: patching the template's own count back every interval would undo every scale
// decision. The count is kept on create, where it is the starting size.
func (r *WorkerVersionSetReconciler) applyDesired(
	ctx context.Context,
	set *dbosv1alpha1.WorkerVersionSet,
	version, image string,
	existing *managedDeployment,
) (*unstructured.Unstructured, error) {
	logger := log.FromContext(ctx)

	var templateScaledObject *unstructured.Unstructured
	for _, kind := range applyOrder {
		manifest, err := render(templateFor(set.Spec, kind), version, image)
		if err != nil {
			return nil, fmt.Errorf("render %s: %w", kind, err)
		}
		stamp(manifest, set.Name, r.Config.ManagedBy, r.Config.VersionLabel, version)
		if kind == kindScaledObject {
			templateScaledObject = manifest.DeepCopy()
		}

		name := manifest.GetName()
		found, err := r.exists(ctx, kind, name)
		if err != nil {
			return nil, err
		}

		created := false
		if found {
			if err := r.patch(ctx, kind, name, patchBody(manifest, kind)); err != nil {
				return nil, err
			}
			if kind == kindDeployment && existing != nil && existing.Annotations[DrainedSinceAnnotation] != "" {
				if err := r.mark(ctx, name, nil); err != nil {
					return nil, err
				}
			}
		} else {
			if err := r.create(ctx, kind, manifest); err != nil {
				return nil, err
			}
			created = true
		}
		logger.Info("apply", "kind", kind, "name", name, "version", version, "image", image, "created", created)
	}

	return templateScaledObject, nil
}

// drain parks one non-desired version, and deletes it once it has been idle for the grace period.
//
// rows == nil means the outstanding count is unknown, and an unknown count never deletes.
//
// The ScaledObject can be missing — deleted out of band, or never created because a prior tick
// failed before it patched. Re-render it from the raw template for this version and merge the
// drain patch over it, rather than patching an object that does not exist and leaving the
// Deployment with no KEDA trigger.
func (r *WorkerVersionSetReconciler) drain(
	ctx context.Context,
	set *dbosv1alpha1.WorkerVersionSet,
	deployment managedDeployment,
	templateScaledObject *unstructured.Unstructured,
	rows *int64,
	now time.Time,
	grace time.Duration,
) (dbosv1alpha1.VersionStatus, error) {
	logger := log.FromContext(ctx)
	entry := versionStatus(deployment.Version, dbosv1alpha1.PhaseDraining, deployment.Image, deployment.Replicas, rows)

	patch, err := drainPatch(templateScaledObject, deployment.Version)
	if err != nil {
		return entry, err
	}

	found, err := r.exists(ctx, kindScaledObject, deployment.Name)
	if err != nil {
		return entry, err
	}
	if found {
		if err := r.patch(ctx, kindScaledObject, deployment.Name, patch); err != nil {
			return entry, err
		}
	} else {
		manifest, err := render(templateFor(set.Spec, kindScaledObject), deployment.Version, "unused")
		if err != nil {
			return entry, err
		}
		manifest.Object = mergePatch(manifest.Object, patch)
		stamp(manifest, set.Name, r.Config.ManagedBy, r.Config.VersionLabel, deployment.Version)
		if err := r.create(ctx, kindScaledObject, manifest); err != nil {
			return entry, err
		}
	}
	logger.Info("drain", "name", deployment.Name, "version", deployment.Version,
		"rows", rowsValue(rows), "pods", deployment.Replicas)

	if rows == nil {
		return entry, nil
	}

	marker := deployment.Annotations[DrainedSinceAnnotation]
	if *rows > 0 || deployment.Replicas > 0 {
		if marker != "" {
			if err := r.mark(ctx, deployment.Name, nil); err != nil {
				return entry, err
			}
			logger.Info("Worker version is busy again; cleared drained marker", "name", deployment.Name)
		}
		return entry, nil
	}

	drainedSince, readable := time.Time{}, false
	if marker != "" {
		drainedSince, readable = parseDrainedSince(marker)
	}
	if !readable {
		if err := r.mark(ctx, deployment.Name, &now); err != nil {
			return entry, err
		}
		logger.Info("Worker version reached zero; marked drained",
			"name", deployment.Name, "version", deployment.Version)
		entry.Phase = dbosv1alpha1.PhaseDrained
		entry.DrainedSince = &metav1.Time{Time: now.UTC().Truncate(time.Second)}
		return entry, nil
	}

	entry.Phase = dbosv1alpha1.PhaseDrained
	entry.DrainedSince = &metav1.Time{Time: drainedSince.Truncate(time.Second)}
	if now.Sub(drainedSince) < grace {
		return entry, nil
	}

	for _, kind := range deleteOrder {
		if err := r.remove(ctx, kind, deployment.Name); err != nil {
			return entry, err
		}
	}
	logger.Info("Deleted drained worker version", "name", deployment.Name, "version", deployment.Version)
	return entry, nil
}

// listManaged returns every worker Deployment of one set, found by label.
func (r *WorkerVersionSetReconciler) listManaged(ctx context.Context, setName string) ([]managedDeployment, error) {
	list := &appsv1.DeploymentList{}
	err := r.List(ctx, list,
		client.InNamespace(r.Config.Namespace),
		client.MatchingLabels{ManagedByLabel: r.Config.ManagedBy, SetLabel: setName},
	)
	if err != nil {
		return nil, err
	}

	managed := make([]managedDeployment, 0, len(list.Items))
	for index := range list.Items {
		item := list.Items[index]
		image := ""
		if containers := item.Spec.Template.Spec.Containers; len(containers) > 0 {
			image = containers[0].Image
		}
		managed = append(managed, managedDeployment{
			Name:        item.Name,
			Version:     item.Labels[r.Config.VersionLabel],
			Replicas:    item.Status.Replicas,
			Annotations: item.Annotations,
			Image:       image,
		})
	}
	return managed, nil
}

// exists reports whether one worker object is present.
func (r *WorkerVersionSetReconciler) exists(ctx context.Context, kind workerKind, name string) (bool, error) {
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(gvkFor[kind])
	err := r.Get(ctx, types.NamespacedName{Namespace: r.Config.Namespace, Name: name}, object)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s %s: %w", kind, name, err)
	}
	return true, nil
}

// create writes one worker object. Dry run logs it instead.
func (r *WorkerVersionSetReconciler) create(
	ctx context.Context,
	kind workerKind,
	manifest *unstructured.Unstructured,
) error {
	logger := log.FromContext(ctx)
	manifest.SetNamespace(r.Config.Namespace)
	if r.Config.DryRun {
		logger.Info("Would create worker object", "kind", kind, "name", manifest.GetName(),
			"namespace", r.Config.Namespace)
		return nil
	}
	if err := r.Create(ctx, manifest.DeepCopy()); err != nil {
		return fmt.Errorf("create %s %s: %w", kind, manifest.GetName(), err)
	}
	return nil
}

// patch merge-patches one worker object. Dry run logs it instead.
//
// Patches are sent as JSON merge patches, so that a patched list — the KEDA spec.triggers — is
// replaced wholesale rather than merged element by element.
func (r *WorkerVersionSetReconciler) patch(
	ctx context.Context,
	kind workerKind,
	name string,
	body map[string]any,
) error {
	logger := log.FromContext(ctx)
	if r.Config.DryRun {
		logger.Info("Would patch worker object", "kind", kind, "name", name, "namespace", r.Config.Namespace)
		return nil
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode %s %s patch: %w", kind, name, err)
	}
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(gvkFor[kind])
	object.SetNamespace(r.Config.Namespace)
	object.SetName(name)
	if err := r.Patch(ctx, object, client.RawPatch(types.MergePatchType, encoded)); err != nil {
		return fmt.Errorf("patch %s %s: %w", kind, name, err)
	}
	return nil
}

// remove deletes one worker object, tolerating an object that is already gone. Dry run logs it
// instead.
func (r *WorkerVersionSetReconciler) remove(ctx context.Context, kind workerKind, name string) error {
	logger := log.FromContext(ctx)
	if r.Config.DryRun {
		logger.Info("Would delete worker object", "kind", kind, "name", name, "namespace", r.Config.Namespace)
		return nil
	}
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(gvkFor[kind])
	object.SetNamespace(r.Config.Namespace)
	object.SetName(name)
	if err := r.Delete(ctx, object); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete %s %s: %w", kind, name, err)
	}
	return nil
}

// mark writes the drained-since marker annotation on one Deployment, or clears it when moment is
// nil.
//
// Like the status subresource, this records what the controller observed rather than acting on
// the cluster, so it is written in dry-run mode too. Without it, the grace clock never advances
// under DVC_DRY_RUN and a dry-run tick past grace never logs "Would delete".
func (r *WorkerVersionSetReconciler) mark(ctx context.Context, name string, moment *time.Time) error {
	var value any
	if moment != nil {
		value = timestamp(*moment)
	}
	body := map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]any{DrainedSinceAnnotation: value},
		},
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode marker patch for %s: %w", name, err)
	}
	object := &appsv1.Deployment{}
	object.SetNamespace(r.Config.Namespace)
	object.SetName(name)
	if err := r.Patch(ctx, object, client.RawPatch(types.MergePatchType, encoded)); err != nil {
		return fmt.Errorf("patch drained marker on %s: %w", name, err)
	}
	return nil
}

// report patches one set's status subresource.
//
// The status is an observation, not an action, so it is written in dry-run mode too: an operator
// trying the controller out still sees what it found.
func (r *WorkerVersionSetReconciler) report(
	ctx context.Context,
	set *dbosv1alpha1.WorkerVersionSet,
	now time.Time,
	versions []dbosv1alpha1.VersionStatus,
	status metav1.ConditionStatus,
	reason, message string,
) {
	logger := log.FromContext(ctx)

	patched := set.DeepCopy()
	patched.Status.ObservedGeneration = set.Generation
	patched.Status.LastReconcileTime = &metav1.Time{Time: now.UTC().Truncate(time.Second)}
	patched.Status.Versions = versions
	setReady(&patched.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: set.Generation,
		LastTransitionTime: metav1.Time{Time: now.UTC().Truncate(time.Second)},
	})

	if err := r.Status().Patch(ctx, patched, client.MergeFrom(set)); err != nil {
		logger.Error(err, "Failed to patch WorkerVersionSet status", "name", set.Name)
	}
}

// setReady writes the Ready condition, keeping its transition time when the status and the reason
// both stayed the same.
func setReady(conditions *[]metav1.Condition, condition metav1.Condition) {
	previous := meta.FindStatusCondition(*conditions, condition.Type)
	if previous != nil && previous.Status == condition.Status && previous.Reason == condition.Reason {
		condition.LastTransitionTime = previous.LastTransitionTime
	}
	meta.RemoveStatusCondition(conditions, condition.Type)
	*conditions = append(*conditions, condition)
}

// versionStatus builds one entry of status.versions, leaving unknown fields out.
func versionStatus(
	version string,
	phase dbosv1alpha1.VersionPhase,
	image string,
	replicas int32,
	rows *int64,
) dbosv1alpha1.VersionStatus {
	entry := dbosv1alpha1.VersionStatus{Version: version, Phase: phase, Replicas: replicas}
	if image != "" {
		entry.Image = image
	}
	if rows != nil {
		outstanding := *rows
		entry.Outstanding = &outstanding
	}
	return entry
}

// rowsValue renders an unknown row count for a log line.
func rowsValue(rows *int64) any {
	if rows == nil {
		return "unknown"
	}
	return *rows
}

func (r *WorkerVersionSetReconciler) now() time.Time {
	if r.Clock != nil {
		return r.Clock()
	}
	return time.Now().UTC()
}

// SetupWithManager sets up the controller with the Manager.
func (r *WorkerVersionSetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&dbosv1alpha1.WorkerVersionSet{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("workerversionset").
		Complete(r)
}
