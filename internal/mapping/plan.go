// Package mapping decides which router actions bring a Service's port
// mappings in line with what it wants. It does no I/O.
package mapping

import (
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/nashant/upnp-nat-controller/internal/upnp"
)

const descriptionPrefix = "upnp-nat-controller/"

// Owner identifies the Service a mapping belongs to.
type Owner struct {
	Namespace string
	Name      string
}

// Desired is one mapping a Service wants, resolved to its target IP.
type Desired struct {
	Protocol       corev1.Protocol
	ExternalPort   uint16
	InternalPort   uint16
	InternalClient string
	LeaseSeconds   uint32
}

func (d Desired) toPortMapping(o Owner) upnp.PortMapping {
	return upnp.PortMapping{
		Protocol:       d.Protocol,
		ExternalPort:   d.ExternalPort,
		InternalPort:   d.InternalPort,
		InternalClient: d.InternalClient,
		Enabled:        true,
		Description:    OwnerDescription(o.Namespace, o.Name),
		LeaseDuration:  d.LeaseSeconds,
	}
}

// Kind is the type of an Action.
type Kind string

// Action kinds.
const (
	Add      Kind = "Add"
	Renew    Kind = "Renew"
	Replace  Kind = "Replace" // Delete Existing, then Add Mapping
	Delete   Kind = "Delete"
	Conflict Kind = "Conflict" // Existing belongs to someone else; nothing is changed
)

// Action is one step of a plan. Mapping is what to write (unset for
// Delete); Existing is the router entry acted on (unset for Add).
type Action struct {
	Kind     Kind
	Mapping  upnp.PortMapping
	Existing *upnp.PortMapping
}

// OwnerDescription is the port mapping description that marks a mapping
// as belonging to a Service.
func OwnerDescription(namespace, name string) string {
	return descriptionPrefix + namespace + "/" + name
}

// ParseOwnerDescription is the inverse of OwnerDescription.
func ParseOwnerDescription(desc string) (namespace, name string, ok bool) {
	rest, found := strings.CutPrefix(desc, descriptionPrefix)
	if !found {
		return "", "", false
	}
	namespace, name, found = strings.Cut(rest, "/")
	if !found || namespace == "" || name == "" || strings.Contains(name, "/") {
		return "", "", false
	}
	return namespace, name, true
}

// legacyDescription is the description the Python controller used.
func legacyDescription(o Owner) string {
	return o.Namespace + "/" + o.Name
}

type key struct {
	proto corev1.Protocol
	port  uint16
}

// Plan returns the actions that make actual (the router's whole table)
// match desired for owner. Mappings owned by owner (current or legacy
// description) that are not desired are deleted; mappings owned by anyone
// else are never touched. A mapping of ours is renewed when its remaining
// lease is below renewBelow. Actions are ordered by protocol, then port.
func Plan(owner Owner, desired []Desired, actual []upnp.PortMapping, renewBelow time.Duration) []Action {
	ownDesc, legacyDesc := OwnerDescription(owner.Namespace, owner.Name), legacyDescription(owner)
	byKey := make(map[key]upnp.PortMapping, len(actual))
	for _, a := range actual {
		byKey[key{a.Protocol, a.ExternalPort}] = a
	}

	var actions []Action
	wanted := make(map[key]bool, len(desired))
	for _, d := range desired {
		k := key{d.Protocol, d.ExternalPort}
		wanted[k] = true
		m := d.toPortMapping(owner)
		existing, found := byKey[k]
		switch {
		case !found:
			actions = append(actions, Action{Kind: Add, Mapping: m})
		case existing.Description == legacyDesc:
			actions = append(actions, Action{Kind: Replace, Mapping: m, Existing: &existing})
		case existing.Description != ownDesc:
			actions = append(actions, Action{Kind: Conflict, Mapping: m, Existing: &existing})
		case existing.InternalClient != m.InternalClient || existing.InternalPort != m.InternalPort || !existing.Enabled:
			actions = append(actions, Action{Kind: Replace, Mapping: m, Existing: &existing})
		case existing.LeaseDuration > 0 && time.Duration(existing.LeaseDuration)*time.Second < renewBelow:
			actions = append(actions, Action{Kind: Renew, Mapping: m, Existing: &existing})
		}
	}

	for _, a := range actual {
		if wanted[key{a.Protocol, a.ExternalPort}] {
			continue
		}
		if a.Description == ownDesc || a.Description == legacyDesc {
			actions = append(actions, Action{Kind: Delete, Existing: &a})
		}
	}

	slices.SortStableFunc(actions, func(a, b Action) int {
		ka, kb := a.target(), b.target()
		return upnp.ComparePortKey(ka.proto, ka.port, kb.proto, kb.port)
	})
	return actions
}

func (a Action) target() key {
	if a.Kind == Delete {
		return key{a.Existing.Protocol, a.Existing.ExternalPort}
	}
	return key{a.Mapping.Protocol, a.Mapping.ExternalPort}
}
