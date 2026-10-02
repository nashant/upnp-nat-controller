// Package annotations parses the advertise.upnp/* Service annotations into
// the set of port mappings a Service wants on the router.
package annotations

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// Annotation keys. The tcp/udp keys are compatible with the Python controller.
const (
	TCPEnabled   = "tcp.advertise.upnp/enabled"
	TCPPorts     = "tcp.advertise.upnp/ports"
	UDPEnabled   = "udp.advertise.upnp/enabled"
	UDPPorts     = "udp.advertise.upnp/ports"
	LeaseSeconds = "advertise.upnp/lease-seconds"
)

// Desired is one port mapping a Service wants on the router.
type Desired struct {
	Protocol     corev1.Protocol
	ExternalPort uint16
	InternalPort uint16
}

// Spec is the parsed annotation set of a managed Service.
type Spec struct {
	Mappings     []Desired
	LeaseSeconds uint32
}

// PortPair is one entry of a ports annotation.
type PortPair struct {
	External uint16
	Internal uint16
}

type protoKeys struct {
	proto   corev1.Protocol
	enabled string
	ports   string
}

var protocols = []protoKeys{
	{corev1.ProtocolTCP, TCPEnabled, TCPPorts},
	{corev1.ProtocolUDP, UDPEnabled, UDPPorts},
}

func enabled(ann map[string]string, key string) bool {
	return strings.EqualFold(ann[key], "true")
}

// IsManaged reports whether any protocol is enabled. "true" is matched
// case-insensitively; any other value, including surrounding whitespace,
// is false.
func IsManaged(ann map[string]string) bool {
	for _, p := range protocols {
		if enabled(ann, p.enabled) {
			return true
		}
	}
	return false
}

// ParsePorts parses a comma-separated list of "N" or "EXT:INT" entries.
// Empty entries are ignored. All errors are reported.
func ParsePorts(s string) ([]PortPair, error) {
	var (
		out  []PortPair
		errs []error
	)
	for _, entry := range strings.Split(s, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		ext, intl, found := strings.Cut(entry, ":")
		if !found {
			intl = ext
		}
		e, err1 := parsePort(ext)
		i, err2 := parsePort(intl)
		if err1 != nil || err2 != nil {
			errs = append(errs, fmt.Errorf("invalid port %q: want N or EXT:INT with ports 1-65535", entry))
			continue
		}
		out = append(out, PortPair{External: e, Internal: i})
	}
	return out, errors.Join(errs...)
}

func parsePort(s string) (uint16, error) {
	n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 16)
	if err != nil || n == 0 {
		return 0, fmt.Errorf("invalid port %q", s)
	}
	return uint16(n), nil
}

// Parse returns the mappings requested by ann, validated against the
// Service's ports. Mappings are sorted by protocol then external port, and
// exact duplicates are dropped. All validation errors are joined.
func Parse(ann map[string]string, ports []corev1.ServicePort, defaultLease uint32) (Spec, error) {
	spec := Spec{LeaseSeconds: defaultLease}
	var errs []error

	if v, ok := ann[LeaseSeconds]; ok {
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: invalid lease %q: want integer 0-%d", LeaseSeconds, v, uint32(1<<32-1)))
		} else {
			spec.LeaseSeconds = uint32(n)
		}
	}

	for _, p := range protocols {
		if !enabled(ann, p.enabled) {
			continue
		}
		pairs, err := ParsePorts(ann[p.ports])
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p.ports, err))
		}
		byExt := map[uint16]uint16{}
		for _, pp := range pairs {
			if prev, seen := byExt[pp.External]; seen {
				if prev != pp.Internal {
					errs = append(errs, fmt.Errorf("%s: duplicate external port %d", p.ports, pp.External))
				}
				continue
			}
			if !hasServicePort(ports, p.proto, pp.Internal) {
				errs = append(errs, fmt.Errorf("%s: internal port %d/%s is not a service port", p.ports, pp.Internal, p.proto))
				continue
			}
			byExt[pp.External] = pp.Internal
			spec.Mappings = append(spec.Mappings, Desired{Protocol: p.proto, ExternalPort: pp.External, InternalPort: pp.Internal})
		}
	}

	if err := errors.Join(errs...); err != nil {
		return Spec{}, err
	}
	slices.SortFunc(spec.Mappings, func(a, b Desired) int {
		return cmp.Or(cmp.Compare(a.Protocol, b.Protocol), cmp.Compare(a.ExternalPort, b.ExternalPort))
	})
	return spec, nil
}

func hasServicePort(ports []corev1.ServicePort, proto corev1.Protocol, port uint16) bool {
	for _, sp := range ports {
		spProto := sp.Protocol
		if spProto == "" {
			spProto = corev1.ProtocolTCP
		}
		if spProto == proto && sp.Port == int32(port) {
			return true
		}
	}
	return false
}

// ValidateLease rejects a non-permanent lease of minSeconds or less: such a
// mapping could expire between two renewal passes.
func (s Spec) ValidateLease(minSeconds uint32) error {
	if s.LeaseSeconds != 0 && s.LeaseSeconds <= minSeconds {
		return fmt.Errorf("%s: lease %ds is too short, want 0 (permanent) or more than %ds", LeaseSeconds, s.LeaseSeconds, minSeconds)
	}
	return nil
}
