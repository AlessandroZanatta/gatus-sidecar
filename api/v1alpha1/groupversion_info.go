// Package v1alpha1 contains the API types for the gatus-sidecar.
// +kubebuilder:object:generate=true
// +groupName=gatus.kalexlab.xyz
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is the group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "gatus.kalexlab.xyz", Version: "v1alpha1"}

	// SchemeBuilder collects the functions that add these types to a Scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

// Resource maps a resource name to a GroupResource in this group.
func Resource(resource string) schema.GroupResource {
	return GroupVersion.WithResource(resource).GroupResource()
}

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, &EndpointTemplate{}, &EndpointTemplateList{})
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}
