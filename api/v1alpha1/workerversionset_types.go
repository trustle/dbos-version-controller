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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// WorkerVersionSetSpec is one DBOS application's desired worker version and the
// manifests that render it.
type WorkerVersionSetSpec struct {
	// version is the desired DBOS application version. The pattern admits no quote character,
	// because the version is inlined into a KEDA query string.
	// +required
	// +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9.-]{0,48}$`
	Version string `json:"version"`

	// image is the worker image the desired version runs.
	// +required
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9./:_@-]+$`
	Image string `json:"image"`

	// graceSeconds is the number of seconds a drained version stays at zero rows and zero pods
	// before it is deleted. Defaults to the controller's DVC_GRACE_SECONDS.
	// +optional
	// +kubebuilder:validation:Minimum=0
	GraceSeconds *int `json:"graceSeconds,omitempty"`

	// template holds the three manifests, each a YAML string carrying the literal __VERSION__ and
	// __IMAGE__ placeholders. All three must share one metadata.name.
	// +required
	Template WorkerVersionSetTemplate `json:"template"`
}

// WorkerVersionSetTemplate holds the three per-version manifests.
type WorkerVersionSetTemplate struct {
	// deployment is the worker Deployment manifest.
	// +required
	Deployment string `json:"deployment"`

	// scaledObject is the KEDA ScaledObject manifest. Its first trigger seeds the drain trigger.
	// +required
	ScaledObject string `json:"scaledObject"`

	// podDisruptionBudget is the PodDisruptionBudget manifest.
	// +required
	PodDisruptionBudget string `json:"podDisruptionBudget"`
}

// VersionPhase is the state of one live version of a set.
// +kubebuilder:validation:Enum=Current;Draining;Drained
type VersionPhase string

const (
	// PhaseCurrent is the desired version.
	PhaseCurrent VersionPhase = "Current"
	// PhaseDraining is an old version KEDA holds at zero.
	PhaseDraining VersionPhase = "Draining"
	// PhaseDrained is an old version at zero rows and zero pods, serving its grace period.
	PhaseDrained VersionPhase = "Drained"
)

// VersionStatus is what the controller saw of one live version.
type VersionStatus struct {
	// version is the DBOS application version.
	// +required
	Version string `json:"version"`

	// phase is Current for the desired version. Draining is an old version KEDA holds at
	// zero. Drained is an old version at zero rows and zero pods, serving its grace period.
	// +required
	Phase VersionPhase `json:"phase"`

	// image is the image of the version's first worker container.
	// +optional
	Image string `json:"image,omitempty"`

	// outstanding is the count of ENQUEUED, DELAYED and PENDING workflow rows of this version.
	// Absent when the database was unreachable.
	// +optional
	Outstanding *int64 `json:"outstanding,omitempty"`

	// replicas is the Deployment's .status.replicas.
	// +optional
	Replicas int32 `json:"replicas"`

	// drainedSince is when the version reached zero. Present only while Drained.
	// +optional
	DrainedSince *metav1.Time `json:"drainedSince,omitempty"`
}

// WorkerVersionSetStatus is what the controller saw on its last reconcile of this set.
type WorkerVersionSetStatus struct {
	// observedGeneration is the .metadata.generation the last reconcile read.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// lastReconcileTime is when the controller last reconciled this set.
	// +optional
	LastReconcileTime *metav1.Time `json:"lastReconcileTime,omitempty"`

	// versions holds one entry per live version of this set, the desired version first.
	// +optional
	Versions []VersionStatus `json:"versions,omitempty"`

	// conditions holds the Ready condition of the last reconcile.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=wvs
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.spec.version`
// +kubebuilder:printcolumn:name="Image",type=string,JSONPath=`.spec.image`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// WorkerVersionSet is the Schema for the workerversionsets API
type WorkerVersionSet struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of WorkerVersionSet
	// +required
	Spec WorkerVersionSetSpec `json:"spec"`

	// status defines the observed state of WorkerVersionSet
	// +optional
	Status WorkerVersionSetStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// WorkerVersionSetList contains a list of WorkerVersionSet
type WorkerVersionSetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []WorkerVersionSet `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &WorkerVersionSet{}, &WorkerVersionSetList{})
		return nil
	})
}
