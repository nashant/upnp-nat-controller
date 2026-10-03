package mapping

import (
	"strconv"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"

	"github.com/nashant/upnp-nat-controller/internal/upnp"
)

const (
	lbIP       = "172.16.1.2"
	renewBelow = 60 * time.Second
)

var (
	owner = Owner{Namespace: "traefik", Name: "public-traefik"}
	names = DefaultDescriptions
)

func want443() Desired {
	return Desired{Protocol: corev1.ProtocolTCP, ExternalPort: 443, InternalPort: 443, InternalClient: lbIP, LeaseSeconds: 3600}
}

// ours returns the router entry that Add(d) would have produced, with the given remaining lease.
func ours(d Desired, remaining uint32) upnp.PortMapping {
	m := d.toPortMapping(names.For(owner))
	m.LeaseDuration = remaining
	return m
}

func TestPlan_Missing_Adds(t *testing.T) {
	got := names.Plan(owner, []Desired{want443()}, nil, renewBelow)
	want := []Action{{Kind: Add, Mapping: ours(want443(), 3600)}}
	if d := cmp.Diff(want, got); d != "" {
		t.Fatalf("(-want +got)\n%s", d)
	}
	if got[0].Mapping.Description != "unc/traefik/public-traefik" || !got[0].Mapping.Enabled {
		t.Fatalf("bad add mapping %+v", got[0].Mapping)
	}
}

func TestPlan_OursMatching_NoOp(t *testing.T) {
	got := names.Plan(owner, []Desired{want443()}, []upnp.PortMapping{ours(want443(), 3000)}, renewBelow)
	if len(got) != 0 {
		t.Fatalf("want no actions, got %+v", got)
	}
}

func TestPlan_OursMatching_PermanentLease_NoOp(t *testing.T) { // router lease 0 = permanent
	got := names.Plan(owner, []Desired{want443()}, []upnp.PortMapping{ours(want443(), 0)}, renewBelow)
	if len(got) != 0 {
		t.Fatalf("want no actions, got %+v", got)
	}
}

func TestPlan_OursMatching_LeaseLow_Renews(t *testing.T) {
	existing := ours(want443(), 59)
	got := names.Plan(owner, []Desired{want443()}, []upnp.PortMapping{existing}, renewBelow)
	want := []Action{{Kind: Renew, Mapping: ours(want443(), 3600), Existing: &existing}}
	if d := cmp.Diff(want, got); d != "" {
		t.Fatalf("(-want +got)\n%s", d)
	}
}

func TestPlan_OursDifferentIP_Replaces(t *testing.T) {
	existing := ours(want443(), 3000)
	existing.InternalClient = "172.16.1.9"
	got := names.Plan(owner, []Desired{want443()}, []upnp.PortMapping{existing}, renewBelow)
	want := []Action{{Kind: Replace, Mapping: ours(want443(), 3600), Existing: &existing}}
	if d := cmp.Diff(want, got); d != "" {
		t.Fatalf("(-want +got)\n%s", d)
	}
}

func TestPlan_OursDifferentInternalPort_Replaces(t *testing.T) {
	existing := ours(want443(), 3000)
	existing.InternalPort = 8443
	got := names.Plan(owner, []Desired{want443()}, []upnp.PortMapping{existing}, renewBelow)
	if len(got) != 1 || got[0].Kind != Replace {
		t.Fatalf("want one Replace, got %+v", got)
	}
}

func TestPlan_OursDisabled_Replaces(t *testing.T) {
	existing := ours(want443(), 3000)
	existing.Enabled = false
	got := names.Plan(owner, []Desired{want443()}, []upnp.PortMapping{existing}, renewBelow)
	if len(got) != 1 || got[0].Kind != Replace {
		t.Fatalf("want one Replace, got %+v", got)
	}
}

func foreign443() upnp.PortMapping {
	return upnp.PortMapping{Protocol: corev1.ProtocolTCP, ExternalPort: 443, InternalPort: 443, InternalClient: "192.168.1.50", Enabled: true, Description: "Xbox"}
}

func TestPlan_Foreign_Conflict(t *testing.T) {
	f := foreign443()
	got := names.Plan(owner, []Desired{want443()}, []upnp.PortMapping{f}, renewBelow)
	want := []Action{{Kind: Conflict, Mapping: ours(want443(), 3600), Existing: &f}}
	if d := cmp.Diff(want, got); d != "" {
		t.Fatalf("(-want +got)\n%s", d)
	}
}

func TestPlan_OtherServiceOwnedDesiredPort_Conflict(t *testing.T) { // ownership is per Service
	other := ours(want443(), 3000)
	other.Description = names.For(Owner{Namespace: "default", Name: "other"})
	got := names.Plan(owner, []Desired{want443()}, []upnp.PortMapping{other}, renewBelow)
	if len(got) != 1 || got[0].Kind != Conflict {
		t.Fatalf("want one Conflict, got %+v", got)
	}
}

func TestPlan_OwnershipByDescriptionNotIP(t *testing.T) {
	// Same IP as ours but foreign description: still a conflict.
	f := foreign443()
	f.InternalClient = lbIP
	got := names.Plan(owner, []Desired{want443()}, []upnp.PortMapping{f}, renewBelow)
	if len(got) != 1 || got[0].Kind != Conflict {
		t.Fatalf("want one Conflict, got %+v", got)
	}
}

func TestPlan_ForeignDoesNotBlockOtherPorts(t *testing.T) {
	d32400 := want443()
	d32400.ExternalPort, d32400.InternalPort = 32400, 32400
	got := names.Plan(owner, []Desired{want443(), d32400}, []upnp.PortMapping{foreign443()}, renewBelow)
	if len(got) != 2 || got[0].Kind != Conflict || got[1].Kind != Add || got[1].Mapping.ExternalPort != 32400 {
		t.Fatalf("want Conflict(443), Add(32400); got %+v", got)
	}
}

func TestPlan_StaleOwned_Deletes(t *testing.T) {
	stale := ours(Desired{Protocol: corev1.ProtocolTCP, ExternalPort: 32400, InternalPort: 32400, InternalClient: lbIP}, 3000)
	got := names.Plan(owner, []Desired{want443()}, []upnp.PortMapping{ours(want443(), 3000), stale}, renewBelow)
	want := []Action{{Kind: Delete, Existing: &stale}}
	if d := cmp.Diff(want, got); d != "" {
		t.Fatalf("(-want +got)\n%s", d)
	}
}

func TestPlan_NoDesired_DeletesAllOwned(t *testing.T) { // unmanage/delete uses an empty desired set
	a, b := ours(want443(), 3000), foreign443()
	b.ExternalPort = 80
	got := names.Plan(owner, nil, []upnp.PortMapping{a, b}, renewBelow)
	want := []Action{{Kind: Delete, Existing: &a}}
	if d := cmp.Diff(want, got); d != "" {
		t.Fatalf("(-want +got)\n%s", d)
	}
}

func TestPlan_OtherServiceOwned_Untouched(t *testing.T) {
	other := ours(Desired{Protocol: corev1.ProtocolTCP, ExternalPort: 80, InternalPort: 80, InternalClient: "172.16.1.3"}, 3000)
	other.Description = names.For(Owner{Namespace: "default", Name: "other"})
	got := names.Plan(owner, []Desired{want443()}, []upnp.PortMapping{ours(want443(), 3000), other}, renewBelow)
	if len(got) != 0 {
		t.Fatalf("want no actions, got %+v", got)
	}
}

func TestPlan_LegacyDescription_Adopts(t *testing.T) {
	legacy := ours(want443(), 604000)
	legacy.Description = "traefik/public-traefik"
	got := names.Plan(owner, []Desired{want443()}, []upnp.PortMapping{legacy}, renewBelow)
	want := []Action{{Kind: Replace, Mapping: ours(want443(), 3600), Existing: &legacy}}
	if d := cmp.Diff(want, got); d != "" {
		t.Fatalf("(-want +got)\n%s", d)
	}
}

func TestPlan_LegacyStale_Deletes(t *testing.T) {
	legacy := ours(Desired{Protocol: corev1.ProtocolTCP, ExternalPort: 80, InternalPort: 80, InternalClient: lbIP}, 604000)
	legacy.Description = "traefik/public-traefik"
	got := names.Plan(owner, nil, []upnp.PortMapping{legacy}, renewBelow)
	if len(got) != 1 || got[0].Kind != Delete {
		t.Fatalf("want one Delete, got %+v", got)
	}
}

func TestPlan_LegacyOtherService_Conflict(t *testing.T) { // only *this* Service's legacy entry is adopted
	legacy := ours(want443(), 604000)
	legacy.Description = "default/other"
	got := names.Plan(owner, []Desired{want443()}, []upnp.PortMapping{legacy}, renewBelow)
	if len(got) != 1 || got[0].Kind != Conflict {
		t.Fatalf("want one Conflict, got %+v", got)
	}
}

func TestPlan_ProtocolsIndependent(t *testing.T) {
	udp := want443()
	udp.Protocol = corev1.ProtocolUDP
	got := names.Plan(owner, []Desired{want443(), udp}, []upnp.PortMapping{ours(want443(), 3000)}, renewBelow)
	if len(got) != 1 || got[0].Kind != Add || got[0].Mapping.Protocol != corev1.ProtocolUDP {
		t.Fatalf("want Add(UDP/443), got %+v", got)
	}
}

func TestPlan_Deterministic_Order(t *testing.T) {
	mk := func(p corev1.Protocol, port uint16) Desired {
		return Desired{Protocol: p, ExternalPort: port, InternalPort: port, InternalClient: lbIP, LeaseSeconds: 3600}
	}
	desired := []Desired{mk(corev1.ProtocolUDP, 53), mk(corev1.ProtocolTCP, 8080), mk(corev1.ProtocolTCP, 22)}
	stale := ours(mk(corev1.ProtocolTCP, 9000), 100)
	actual := []upnp.PortMapping{stale}
	first := names.Plan(owner, desired, actual, renewBelow)
	for i := 0; i < 20; i++ {
		// reverse inputs: output must not depend on input order
		rev := []Desired{desired[2], desired[1], desired[0]}
		if d := cmp.Diff(first, names.Plan(owner, rev, actual, renewBelow)); d != "" {
			t.Fatalf("non-deterministic (-first +now)\n%s", d)
		}
	}
	var keys []string
	for _, a := range first {
		m := a.Mapping
		if a.Kind == Delete {
			m = *a.Existing
		}
		keys = append(keys, string(m.Protocol)+":"+strconv.Itoa(int(m.ExternalPort)))
	}
	if d := cmp.Diff([]string{"TCP:22", "TCP:8080", "TCP:9000", "UDP:53"}, keys); d != "" {
		t.Fatalf("order (-want +got)\n%s", d)
	}
}

func TestPlan_RenewsBelowHalfLease(t *testing.T) { // renew at lease/2
	existing := ours(want443(), 1799) // requested 3600
	got := names.Plan(owner, []Desired{want443()}, []upnp.PortMapping{existing}, renewBelow)
	if len(got) != 1 || got[0].Kind != Renew {
		t.Fatalf("want Renew below half-lease, got %+v", got)
	}
	if got := names.Plan(owner, []Desired{want443()}, []upnp.PortMapping{ours(want443(), 1801)}, renewBelow); len(got) != 0 {
		t.Fatalf("want no-op above half-lease, got %+v", got)
	}
}

func TestPlan_ShortLeaseUsesRenewBelow(t *testing.T) {
	d := want443()
	d.LeaseSeconds = 100 // half is 50s, below renewBelow (60s)
	if got := names.Plan(owner, []Desired{d}, []upnp.PortMapping{ours(d, 59)}, renewBelow); len(got) != 1 || got[0].Kind != Renew {
		t.Fatalf("want Renew below renewBelow, got %+v", got)
	}
}

func TestPlan_LeaseLowered_Renews(t *testing.T) {
	d := want443()
	d.LeaseSeconds = 600
	existing := ours(want443(), 3000) // still holds the old 3600 lease
	got := names.Plan(owner, []Desired{d}, []upnp.PortMapping{existing}, renewBelow)
	if len(got) != 1 || got[0].Kind != Renew || got[0].Mapping.LeaseDuration != 600 {
		t.Fatalf("want Renew with the new lease, got %+v", got)
	}
}

func TestPlan_AdoptsFormerPrefix(t *testing.T) { // cutover from the upnp-nat-controller/ prefix
	old := ours(want443(), 3000)
	old.Description = "upnp-nat-controller/traefik/public-traefik"
	got := names.Plan(owner, []Desired{want443()}, []upnp.PortMapping{old}, renewBelow)
	if len(got) != 1 || got[0].Kind != Replace || got[0].Mapping.Description != "unc/traefik/public-traefik" {
		t.Fatalf("want Replace to the new description, got %+v", got)
	}
	stale := old
	stale.ExternalPort = 80
	if got := names.Plan(owner, nil, []upnp.PortMapping{stale}, renewBelow); len(got) != 1 || got[0].Kind != Delete {
		t.Fatalf("want Delete of stale former-prefix mapping, got %+v", got)
	}
}

func TestPlan_LongOwnerFitsRouterLabel(t *testing.T) {
	long := Owner{Namespace: "a-rather-long-namespace-name", Name: "and-an-equally-long-service-name"}
	d := want443()
	m := d.toPortMapping(names.For(long))
	if len(m.Description) > MaxDescriptionLen {
		t.Fatalf("description %q is %d bytes", m.Description, len(m.Description))
	}
	// Router stores at most 63 bytes; the stored value must still match.
	stored := m
	stored.Description = stored.Description[:min(len(stored.Description), MaxDescriptionLen)]
	stored.LeaseDuration = 3000
	if got := names.Plan(long, []Desired{d}, []upnp.PortMapping{stored}, renewBelow); len(got) != 0 {
		t.Fatalf("own long-named mapping not recognised: %+v", got)
	}
}
