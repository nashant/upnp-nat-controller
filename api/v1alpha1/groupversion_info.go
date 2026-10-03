// Package v1alpha1 contains API types for the gateway.nashes.uk group.
// +kubebuilder:object:generate=true
// +groupName=gateway.nashes.uk
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is the group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "gateway.nashes.uk", Version: "v1alpha1"}

	// SchemeBuilder adds the types to a scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme adds the types in this group-version to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, &InternetGatewayDevice{}, &InternetGatewayDeviceList{})
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}
