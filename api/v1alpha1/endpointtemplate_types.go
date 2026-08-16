package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EndpointTemplateSpec defines a reusable fragment of Gatus endpoint configuration.
// It is the replacement for YAML anchors in a hand-maintained Gatus config.
type EndpointTemplateSpec struct {
	// Extends lists templates merged in before this one, in order. Later entries
	// win over earlier ones, and this template's own Endpoint wins over all of them.
	// Cycles are rejected and mark the template not Ready.
	// +optional
	Extends []string `json:"extends,omitempty"`

	// DefaultFor makes this template apply automatically to every discovered
	// endpoint whose resolved scheme appears in this list, without the workload
	// having to reference it. An object that sets the "template" annotation
	// replaces this automatic set entirely.
	// +optional
	DefaultFor []string `json:"defaultFor,omitempty"`

	// Scheme is the URL scheme endpoints using this template default to when the
	// workload does not specify one. It also drives URL derivation (http, https, tcp).
	// +optional
	Scheme string `json:"scheme,omitempty"`

	// Endpoint holds arbitrary Gatus endpoint fields. It is not validated against
	// Gatus's own schema: content is preserved verbatim and deep-merged, so fields
	// added by future Gatus versions work without changing this CRD.
	// +optional
	// +kubebuilder:validation:Type=object
	// +kubebuilder:pruning:PreserveUnknownFields
	Endpoint *apiextensionsv1.JSON `json:"endpoint,omitempty"`
}

// EndpointTemplateStatus reports resolution results. It is informational only:
// a template that fails to resolve is skipped by the renderer rather than
// blocking the rest of the configuration.
type EndpointTemplateStatus struct {
	// ObservedGeneration is the .metadata.generation this status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// UsedBy is the number of rendered endpoints that resolved this template
	// as of the last render.
	// +optional
	UsedBy int32 `json:"usedBy,omitempty"`

	// Conditions holds the "Ready" condition, false when extends cannot be
	// resolved or forms a cycle.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Condition types and reasons reported in EndpointTemplateStatus.
const (
	// ConditionReady is true when the template and its extends chain resolve.
	ConditionReady = "Ready"

	// ReasonResolved is the Ready=true reason.
	ReasonResolved = "Resolved"
	// ReasonUnknownParent is set when an entry in extends names a missing template.
	ReasonUnknownParent = "UnknownParent"
	// ReasonCycle is set when the extends chain is cyclic.
	ReasonCycle = "Cycle"
	// ReasonInvalidEndpoint is set when the endpoint body is not a JSON object.
	ReasonInvalidEndpoint = "InvalidEndpoint"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=etpl,categories=gatus
// +kubebuilder:printcolumn:name="Scheme",type=string,JSONPath=`.spec.scheme`
// +kubebuilder:printcolumn:name="Default For",type=string,JSONPath=`.spec.defaultFor`
// +kubebuilder:printcolumn:name="Used By",type=integer,JSONPath=`.status.usedBy`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// EndpointTemplate is a cluster-scoped, reusable block of Gatus endpoint config.
type EndpointTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   EndpointTemplateSpec   `json:"spec,omitempty"`
	Status EndpointTemplateStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// EndpointTemplateList contains a list of EndpointTemplate.
type EndpointTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []EndpointTemplate `json:"items"`
}
