// Package ipclass classifies a router's WAN address to detect double NAT.
package ipclass

import "net/netip"

// Class is the kind of address.
type Class string

// Address classes.
const (
	Empty   Class = "Empty"
	Private Class = "Private" // RFC 1918, loopback, link-local
	CGNAT   Class = "CGNAT"   // 100.64.0.0/10 (RFC 6598)
	Public  Class = "Public"
)

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// ClassifyExternalIP classifies the router's reported external address.
func ClassifyExternalIP(s string) Class {
	ip, err := netip.ParseAddr(s)
	if err != nil || ip.IsUnspecified() {
		return Empty
	}
	ip = ip.Unmap()
	switch {
	case cgnat.Contains(ip):
		return CGNAT
	case ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast():
		return Private
	}
	return Public
}

// IsPublic reports whether c is a publicly routable address.
func (c Class) IsPublic() bool { return c == Public }
