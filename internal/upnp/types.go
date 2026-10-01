// Package upnp talks to a UPnP Internet Gateway Device.
package upnp

import (
	"cmp"

	corev1 "k8s.io/api/core/v1"
)

// PortMapping is one entry in the router's port mapping table.
type PortMapping struct {
	Protocol       corev1.Protocol
	ExternalPort   uint16
	InternalPort   uint16
	InternalClient string
	Enabled        bool
	Description    string
	// LeaseDuration is the requested lease on Add, and the remaining lease
	// when read back from the router. 0 means permanent.
	LeaseDuration uint32
}

// ComparePortKey orders mappings by protocol, then external port.
func ComparePortKey(protoA corev1.Protocol, portA uint16, protoB corev1.Protocol, portB uint16) int {
	return cmp.Or(cmp.Compare(protoA, protoB), cmp.Compare(portA, portB))
}
