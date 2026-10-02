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
	"fmt"
	"maps"
	"regexp"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"

	dbosv1alpha1 "github.com/trustle/dbos-version-controller/api/v1alpha1"
)

// workerKind is one of the three object kinds a version owns.
type workerKind string

const (
	kindDeployment   workerKind = "deployment"
	kindScaledObject workerKind = "scaledobject"
	kindPDB          workerKind = "pdb"
)

// applyOrder is the order the desired version's objects are applied in.
var applyOrder = []workerKind{kindDeployment, kindScaledObject, kindPDB}

// deleteOrder removes the scaler first, so nothing scales the version back up mid-delete.
var deleteOrder = []workerKind{kindScaledObject, kindPDB, kindDeployment}

var gvkFor = map[workerKind]schema.GroupVersionKind{
	kindDeployment:   {Group: "apps", Version: "v1", Kind: "Deployment"},
	kindScaledObject: {Group: "keda.sh", Version: "v1alpha1", Kind: "ScaledObject"},
	kindPDB:          {Group: "policy", Version: "v1", Kind: "PodDisruptionBudget"},
}

// Labels and annotations the controller owns.
const (
	// ManagedByLabel names the controller that owns an object.
	ManagedByLabel = "app.kubernetes.io/managed-by"
	// SetLabel names the WorkerVersionSet an object belongs to.
	SetLabel = "dbos.trustle.com/worker-version-set"
	// DrainedSinceAnnotation records when a version reached zero rows and zero pods.
	DrainedSinceAnnotation = "dbos-version-controller/drained-since"
)

// The reasons of the Ready condition.
const (
	// ReasonReconciled means the set's versions are in the state its spec asks for.
	ReasonReconciled = "Reconciled"
	// ReasonInvalidSpec means the spec is unusable, so the controller wrote nothing for the set.
	ReasonInvalidSpec = "InvalidSpec"
	// ReasonDatabaseUnavailable means the outstanding count is unknown, so nothing was deleted.
	ReasonDatabaseUnavailable = "DatabaseUnavailable"
	// ReasonPodsUnavailable means the pods that carry a version could not be counted, so nothing was
	// deleted for those versions.
	ReasonPodsUnavailable = "PodsUnavailable"
	// ReasonDrainFailed names the versions whose drain failed.
	ReasonDrainFailed = "DrainFailed"
	// ReasonReconcileError means the reconcile of the whole set failed.
	ReasonReconcileError = "ReconcileError"
)

// timestampLayout renders one RFC 3339 instant the way Kubernetes writes its own timestamps.
const timestampLayout = "2006-01-02T15:04:05Z"

var (
	versionPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,48}$`)
	imagePattern   = regexp.MustCompile(`^[A-Za-z0-9./:_@-]+$`)
)

// OutstandingSQL counts the unfinished workflows of one application version. The version is
// bound as a parameter.
const OutstandingSQL = "SELECT count(*) FROM dbos.workflow_status " +
	"WHERE application_version = $1 AND status IN ('ENQUEUED','PENDING')"

// outstandingSQLFor returns the outstanding-workflow query with the version inlined, for a KEDA
// trigger.
//
// KEDA carries no parameter binding, so the version is substituted literally. Every caller
// validates it against versionPattern first, which admits no quote character.
func outstandingSQLFor(version string) string {
	return fmt.Sprintf(
		"SELECT count(*) FROM dbos.workflow_status "+
			"WHERE application_version = '%s' AND status IN ('ENQUEUED','PENDING')",
		version,
	)
}

// render substitutes the two placeholders and parses the result as a manifest.
func render(template, version, image string) (*unstructured.Unstructured, error) {
	substituted := strings.ReplaceAll(template, "__VERSION__", version)
	substituted = strings.ReplaceAll(substituted, "__IMAGE__", image)

	asJSON, err := yaml.YAMLToJSON([]byte(substituted))
	if err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	manifest := &unstructured.Unstructured{}
	if err := manifest.UnmarshalJSON(asJSON); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	if manifest.GetName() == "" {
		return nil, fmt.Errorf("manifest has no metadata.name")
	}
	return manifest, nil
}

// stamp forces the controller's own labels onto one rendered manifest.
//
// Ownership is label-based, not an ownerReference: deleting and re-applying the
// WorkerVersionSet (Argo prune/recreate, kubectl replace --force) must not garbage-collect a
// draining Deployment with work in flight. Deletion stays the controller's own decision. A set
// re-created with the same name adopts its existing objects by label.
//
// A Deployment carries the version label and the managed-by label on its pod template too: the
// cleanup job and the KEDA queries select pods, not Deployments. spec.selector is left alone,
// because it is immutable on a live Deployment.
func stamp(manifest *unstructured.Unstructured, setName, managedBy, versionLabel, version string) {
	labels := manifest.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[ManagedByLabel] = managedBy
	labels[versionLabel] = version
	labels[SetLabel] = setName
	manifest.SetLabels(labels)

	if manifest.GetKind() != "Deployment" {
		return
	}
	podLabels, _, err := unstructured.NestedStringMap(manifest.Object, podLabelPath...)
	if err != nil || podLabels == nil {
		podLabels = map[string]string{}
	}
	podLabels[ManagedByLabel] = managedBy
	podLabels[versionLabel] = version
	if err := unstructured.SetNestedStringMap(manifest.Object, podLabels, podLabelPath...); err != nil {
		// The only failure is a pod template that is not an object, which the API server rejects
		// on its own with a clearer message than this function could give.
		return
	}
}

// metadataField is the object-meta key of a nested field path.
const metadataField = "metadata"

// podLabelPath locates the pod template labels of a Deployment.
var podLabelPath = []string{"spec", "template", metadataField, "labels"}

// patchBody returns the manifest to send as the patch of an object that already exists.
//
// A live Deployment keeps the replica count it has: KEDA owns that count, and re-sending the
// template's own count every interval would undo every scale decision. The count stays on create,
// where it is the version's starting size. Every other kind is patched whole.
func patchBody(manifest *unstructured.Unstructured, kind workerKind) map[string]any {
	if kind != kindDeployment {
		return manifest.Object
	}
	trimmed := manifest.DeepCopy()
	unstructured.RemoveNestedField(trimmed.Object, "spec", "replicas")
	return trimmed.Object
}

// drainPatch builds the ScaledObject patch that parks a draining version.
//
// The version keeps exactly one trigger: the outstanding-workflow count for its own version,
// targeting one workflow per pod. Together with minReplicaCount: 0 that holds the version at zero
// pods until work of its own version appears, and returns it to zero when that work finishes.
func drainPatch(templateScaledObject *unstructured.Unstructured, version string) (map[string]any, error) {
	triggers, found, err := unstructured.NestedSlice(templateScaledObject.Object, "spec", "triggers")
	if err != nil || !found || len(triggers) == 0 {
		return nil, fmt.Errorf("template ScaledObject has no spec.triggers")
	}
	trigger, ok := triggers[0].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("template ScaledObject's first trigger is not an object")
	}
	triggerType, ok := trigger["type"].(string)
	if !ok {
		return nil, fmt.Errorf("template ScaledObject's first trigger has no type")
	}

	metadata := map[string]any{}
	if existing, ok := trigger["metadata"].(map[string]any); ok {
		for key, value := range existing {
			if key == "query" || key == "targetQueryValue" {
				continue
			}
			metadata[key] = value
		}
	}
	metadata["query"] = outstandingSQLFor(version)
	metadata["targetQueryValue"] = "1"

	drained := map[string]any{
		"type":       triggerType,
		"metricType": "AverageValue",
		"metadata":   metadata,
	}
	if authenticationRef, ok := trigger["authenticationRef"]; ok && authenticationRef != nil {
		drained["authenticationRef"] = authenticationRef
	}

	return map[string]any{
		"spec": map[string]any{
			"minReplicaCount": int64(0),
			"triggers":        []any{drained},
		},
	}, nil
}

// mergePatch applies a JSON merge patch (RFC 7396) onto a manifest, returning a new map.
func mergePatch(manifest, patch map[string]any) map[string]any {
	result := make(map[string]any, len(manifest))
	maps.Copy(result, manifest)
	for key, value := range patch {
		if value == nil {
			delete(result, key)
			continue
		}
		patchChild, patchIsMap := value.(map[string]any)
		resultChild, resultIsMap := result[key].(map[string]any)
		if patchIsMap && resultIsMap {
			result[key] = mergePatch(resultChild, patchChild)
			continue
		}
		result[key] = value
	}
	return result
}

// specProblem returns why a set's spec is unusable, or an empty string when it is usable.
func specProblem(spec dbosv1alpha1.WorkerVersionSetSpec) string {
	version := strings.TrimSpace(spec.Version)
	image := strings.TrimSpace(spec.Image)
	if !versionPattern.MatchString(version) {
		return fmt.Sprintf("spec.version %q does not match %s", version, versionPattern.String())
	}
	if !imagePattern.MatchString(image) {
		return fmt.Sprintf("spec.image %q does not match %s", image, imagePattern.String())
	}
	var missing []string
	for _, candidate := range []struct {
		key   string
		value string
	}{
		{"deployment", spec.Template.Deployment},
		{"scaledObject", spec.Template.ScaledObject},
		{"podDisruptionBudget", spec.Template.PodDisruptionBudget},
	} {
		if strings.TrimSpace(candidate.value) == "" {
			missing = append(missing, candidate.key)
		}
	}
	if len(missing) > 0 {
		return fmt.Sprintf("spec.template is missing %s", strings.Join(missing, ", "))
	}
	return ""
}

// templateFor returns the raw manifest template of one kind.
func templateFor(spec dbosv1alpha1.WorkerVersionSetSpec, kind workerKind) string {
	switch kind {
	case kindDeployment:
		return spec.Template.Deployment
	case kindScaledObject:
		return spec.Template.ScaledObject
	default:
		return spec.Template.PodDisruptionBudget
	}
}

// parseDrainedSince reads the drained-since marker, returning false when it is unreadable.
func parseDrainedSince(value string) (time.Time, bool) {
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.999999999",
		"2006-01-02T15:04:05",
	}
	for _, layout := range layouts {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC(), true
		}
	}
	return time.Time{}, false
}

// timestamp renders one instant the way Kubernetes writes its own timestamps.
func timestamp(moment time.Time) string {
	return moment.UTC().Format(timestampLayout)
}
