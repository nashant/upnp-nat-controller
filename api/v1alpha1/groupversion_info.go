// Package v1alpha1 contains API types for the gateway.nashes.uk group.
// +kubebuilder:object:generate=true
// +groupName=gateway.nashes.uk
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "gateway.nashes.uk", Version: "v1alpha1"}

	// SchemeBuilder adds the types to a scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this group-version to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
