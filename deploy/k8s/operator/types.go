// Package operator implements the K8s Operator (design 05 §8).
// Uses controller-runtime to manage UniSearchCluster CRD with a single
// Reconciler that handles meta/coordinator/shard StatefulSets.
// @spec: ./active_specs/2026-10-20-gap-fill/02_spec.md#2.5
package operator

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// UniSearchCluster is the CRD type for managing a UniSearch cluster.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
type UniSearchCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   UniSearchClusterSpec   `json:"spec,omitempty"`
	Status UniSearchClusterStatus `json:"status,omitempty"`
}

// UniSearchClusterSpec defines the desired state of a UniSearch cluster.
type UniSearchClusterSpec struct {
	MetaReplicas        int32       `json:"metaReplicas"`
	CoordinatorReplicas int32       `json:"coordinatorReplicas"`
	ShardReplicas       int32       `json:"shardReplicas"`
	Image               string      `json:"image"`
	Storage             StorageSpec `json:"storage"`
}

// StorageSpec defines storage configuration.
type StorageSpec struct {
	ObjectStoreEndpoint string `json:"objectStoreEndpoint"`
	AccessKey           string `json:"accessKey"`
	SecretKey           string `json:"secretKey"`
	Bucket              string `json:"bucket"`
}

// UniSearchClusterStatus is the observed state.
type UniSearchClusterStatus struct {
	Phase      string `json:"phase"`  // "pending" | "running" | "failed"
	Ready      bool   `json:"ready"`
	LeaderAddr string `json:"leaderAddr"`
}

// UniSearchClusterList is a list of UniSearchCluster.
// +kubebuilder:object:root=true
type UniSearchClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []UniSearchCluster `json:"items"`
}

// DeepCopyInto implements runtime.Object DeepCopyInto.
func (in *UniSearchCluster) DeepCopyInto(out *UniSearchCluster) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = in.Spec
	out.Status = in.Status
}

// DeepCopy implements runtime.Object DeepCopy.
func (in *UniSearchCluster) DeepCopy() *UniSearchCluster {
	if in == nil {
		return nil
	}
	out := new(UniSearchCluster)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject implements runtime.Object DeepCopyObject.
func (in *UniSearchCluster) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto for UniSearchClusterList.
func (in *UniSearchClusterList) DeepCopyInto(out *UniSearchClusterList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]UniSearchCluster, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

// DeepCopy for UniSearchClusterList.
func (in *UniSearchClusterList) DeepCopy() *UniSearchClusterList {
	if in == nil {
		return nil
	}
	out := new(UniSearchClusterList)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject for UniSearchClusterList.
func (in *UniSearchClusterList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// CRD YAML for K8s deployment.
const CRDYAML = `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: unisearchclusters.unisearch.db
spec:
  group: unisearch.db
  names:
    kind: UniSearchCluster
    plural: unisearchclusters
    singular: unisearchcluster
  scope: Namespaced
  versions:
  - name: v1alpha1
    served: true
    storage: true
    subresources:
      status: {}
    schema:
      openAPIV3Schema:
        type: object
        properties:
          spec:
            type: object
            properties:
              metaReplicas:
                type: integer
                default: 3
              coordinatorReplicas:
                type: integer
                default: 2
              shardReplicas:
                type: integer
                default: 3
              image:
                type: string
                default: "unisearch:latest"
              storage:
                type: object
                properties:
                  objectStoreEndpoint:
                    type: string
                  accessKey:
                    type: string
                  secretKey:
                    type: string
                  bucket:
                    type: string
          status:
            type: object
            properties:
              phase:
                type: string
              ready:
                type: boolean
              leaderAddr:
                type: string
`
