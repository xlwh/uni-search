// Package operator — scheme registration for the UniSearchCluster CRD.
// @spec: ./active_specs/2026-10-20-gap-fill/02_spec.md#2.5
package operator

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupVersion is the API group-version for the UniSearchCluster CRD.
const Group = "unisearch.db"
const Version = "v1alpha1"

// SchemeGroupVersion is the group-version for the CRD.
var SchemeGroupVersion = schema.GroupVersion{Group: Group, Version: Version}

// AddToScheme adds the UniSearchCluster types to the given scheme.
func AddToScheme(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(SchemeGroupVersion, &UniSearchCluster{}, &UniSearchClusterList{})
	return nil
}
