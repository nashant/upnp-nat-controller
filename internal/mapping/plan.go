// Package mapping decides which router actions bring a Service's port
// mappings in line with what it wants. It does no I/O.
package mapping

import (
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/nashant/upnp-nat-controller/internal/upnp"
)

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

func (d Desired) toPortMapping(desc string) upnp.PortMapping {
	return upnp.PortMapping{
		Protocol:       d.Protocol,
		ExternalPort:   d.ExternalPort,
		InternalPort:   d.InternalPort,
		InternalClient: d.InternalClient,
		Enabled:        true,
		Description:    desc,
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

type key struct {
	proto corev1.Protocol
	port  uint16
}

// Plan returns the actions that make actual (the router's whole table)
// match desired for owner. Mappings owned by owner (under the current or an
// adopted description) that are not desired are deleted; adopted ones that
// are desired are rewritten; mappings owned by anyone else are never
// touched. A mapping of ours is renewed once its remaining lease drops
// below renewBelow or half the requested lease, whichever is longer, or when
// it holds a longer lease than now requested. Actions are ordered by
// protocol, then port.
func (d Descriptions) Plan(owner Owner, desired []Desired, actual []upnp.PortMapping, renewBelow time.Duration) []Action {
	ownDesc := d.For(owner)
	byKey := make(map[key]upnp.PortMapping, len(actual))
	for _, a := range actual {
		byKey[key{a.Protocol, a.ExternalPort}] = a
	}

	var actions []Action
	wanted := make(map[key]bool, len(desired))
	for _, want := range desired {
		k := key{want.Protocol, want.ExternalPort}
		wanted[k] = true
		m := want.toPortMapping(ownDesc)
		existing, found := byKey[k]
		if !found {
			actions = append(actions, Action{Kind: Add, Mapping: m})
			continue
		}
		switch d.Owns(owner, existing.Description) {
		case NotOwned:
			actions = append(actions, Action{Kind: Conflict, Mapping: m, Existing: &existing})
		case Adoptable:
			actions = append(actions, Action{Kind: Replace, Mapping: m, Existing: &existing})
		case Current:
			switch {
			case existing.InternalClient != m.InternalClient || existing.InternalPort != m.InternalPort || !existing.Enabled:
				actions = append(actions, Action{Kind: Replace, Mapping: m, Existing: &existing})
			case needsRenew(existing.LeaseDuration, want.LeaseSeconds, renewBelow):
				actions = append(actions, Action{Kind: Renew, Mapping: m, Existing: &existing})
			}
		}
	}

	for _, a := range actual {
		if !wanted[key{a.Protocol, a.ExternalPort}] && d.Owns(owner, a.Description) != NotOwned {
			actions = append(actions, Action{Kind: Delete, Existing: &a})
		}
	}

	slices.SortStableFunc(actions, func(a, b Action) int {
		ka, kb := a.target(), b.target()
		return upnp.ComparePortKey(ka.proto, ka.port, kb.proto, kb.port)
	})
	return actions
}

// needsRenew decides on a mapping of ours. remaining 0 is a permanent
// mapping, which is left alone.
func needsRenew(remaining, requested uint32, renewBelow time.Duration) bool {
	if remaining == 0 {
		return false
	}
	if requested > 0 && remaining > requested {
		return true // the requested lease was lowered
	}
	threshold := max(renewBelow, time.Duration(requested)*time.Second/2)
	return time.Duration(remaining)*time.Second < threshold
}

func (a Action) target() key {
	if a.Kind == Delete {
		return key{a.Existing.Protocol, a.Existing.ExternalPort}
	}
	return key{a.Mapping.Protocol, a.Mapping.ExternalPort}
}
