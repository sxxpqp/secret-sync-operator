package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SecretSyncSpec defines the desired state of SecretSync.
type SecretSyncSpec struct {
	// SourceSecretName is the name of the Secret to copy. It must live in the
	// same namespace as the SecretSync object.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	SourceSecretName string `json:"sourceSecretName"`

	// TargetNamespaces lists the namespaces the Secret is copied into. Every
	// namespace must already exist; the operator never creates namespaces.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=100
	// +listType=set
	TargetNamespaces []string `json:"targetNamespaces"`
}

// SecretSyncStatus defines the observed state of SecretSync.
type SecretSyncStatus struct {
	// ObservedGeneration reflects the generation most recently reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// SyncedNamespaces lists the namespaces synchronized during the last reconcile.
	// +optional
	SyncedNamespaces []string `json:"syncedNamespaces,omitempty"`

	// Conditions represent the latest available observations of the SecretSync's state.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=ssync
// +kubebuilder:printcolumn:name="Source",type=string,JSONPath=`.spec.sourceSecretName`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// SecretSync is the Schema for the secretsyncs API.
type SecretSync struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SecretSyncSpec   `json:"spec"`
	Status SecretSyncStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SecretSyncList contains a list of SecretSync.
type SecretSyncList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SecretSync `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SecretSync{}, &SecretSyncList{})
}
