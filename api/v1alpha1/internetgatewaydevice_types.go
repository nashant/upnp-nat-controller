package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Condition types.
const (
	ConditionDiscovered       = "Discovered"
	ConditionReachable        = "Reachable"
	ConditionConnected        = "Connected"
	ConditionPublicExternalIP = "PublicExternalIP"
	ConditionReady            = "Ready"
)

// InternetGatewayDeviceSpec configures how the router is polled.
type InternetGatewayDeviceSpec struct {
	// PollingInterval is how often, in seconds, the router is polled.
	// +kubebuilder:default=30
	// +kubebuilder:validation:Minimum=10
	// +optional
	PollingInterval int32 `json:"pollingInterval,omitempty"`
}

// ServiceRef identifies the Service a port mapping belongs to.
type ServiceRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// PortMappingStatus is a port mapping owned by this controller.
type PortMappingStatus struct {
	// +kubebuilder:validation:Enum=TCP;UDP
	Protocol       string `json:"protocol"`
	ExternalPort   int32  `json:"externalPort"`
	InternalPort   int32  `json:"internalPort"`
	InternalClient string `json:"internalClient,omitempty"`
	Enabled        bool   `json:"enabled"`
	Description    string `json:"description,omitempty"`
	// LeaseDuration is the remaining lease in seconds; 0 is permanent.
	LeaseDuration int64      `json:"leaseDuration,omitempty"`
	ServiceRef    ServiceRef `json:"serviceRef"`
}

// InternetGatewayDeviceStatus is the observed state of the router.
type InternetGatewayDeviceStatus struct {
	FriendlyName     string `json:"friendlyName,omitempty"`
	Location         string `json:"location,omitempty"`
	InternalIP       string `json:"internalIP,omitempty"`
	ExternalIP       string `json:"externalIP,omitempty"`
	ConnectionStatus string `json:"connectionStatus,omitempty"`
	// Uptime is the WAN connection uptime in seconds.
	Uptime int64 `json:"uptime,omitempty"`
	// LastSeen is when the router last answered a poll.
	// +optional
	LastSeen *metav1.Time `json:"lastSeen,omitempty"`
	// PortMappings are the mappings owned by this controller.
	// +optional
	PortMappings []PortMappingStatus `json:"portMappings,omitempty"`
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// InternetGatewayDevice is the router the controller manages port mappings
// on. It is created by the controller; there is one, named "default".
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=igd
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Device",type=string,JSONPath=`.status.friendlyName`
// +kubebuilder:printcolumn:name="Internal IP",type=string,JSONPath=`.status.internalIP`
// +kubebuilder:printcolumn:name="External IP",type=string,JSONPath=`.status.externalIP`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Uptime",type=integer,JSONPath=`.status.uptime`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type InternetGatewayDevice struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   InternetGatewayDeviceSpec   `json:"spec,omitempty"`
	Status InternetGatewayDeviceStatus `json:"status,omitempty"`
}

// InternetGatewayDeviceList is a list of InternetGatewayDevice.
// +kubebuilder:object:root=true
type InternetGatewayDeviceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []InternetGatewayDevice `json:"items"`
}
