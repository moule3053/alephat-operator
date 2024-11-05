package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MultiClusterResourceSpec defines the desired state of MultiClusterResource
type MultiClusterResourceSpec struct {
	TargetClusters   string            `json:"targetClusters,omitempty"`
	PlacementPolicy  string            `json:"placementPolicy,omitempty"`
	ReplicasOverride int32             `json:"replicasOverride,omitempty"`
	ResourceManifest map[string]string `json:"resourceManifest"`
}

// MultiClusterResourceStatus defines the observed state of MultiClusterResource
type MultiClusterResourceStatus struct {
	TargetClusters   []string           `json:"targetClusters,omitempty"`
	Conditions       []metav1.Condition `json:"conditions,omitempty"`
	DeployedClusters []string           `json:"deployedClusters,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// MultiClusterResource is the Schema for the multiclusterresources API
type MultiClusterResource struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MultiClusterResourceSpec   `json:"spec,omitempty"`
	Status MultiClusterResourceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// MultiClusterResourceList contains a list of MultiClusterResource
type MultiClusterResourceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MultiClusterResource `json:"items"`
}

func init() {
	SchemeBuilder.Register(&MultiClusterResource{}, &MultiClusterResourceList{})
}
