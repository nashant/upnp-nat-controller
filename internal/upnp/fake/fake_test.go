package fake_test

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/nashant/upnp-nat-controller/internal/upnp"
	"github.com/nashant/upnp-nat-controller/internal/upnp/fake"
)

var ctx = context.Background()

func TestFake_RouterSemantics(t *testing.T) {
	c := fake.New()
	var _ upnp.Client = c
	m := upnp.PortMapping{Protocol: corev1.ProtocolTCP, ExternalPort: 443, InternalPort: 443, InternalClient: "172.16.1.2", Enabled: true, Description: "d", LeaseDuration: 3600}
	if err := c.Add(ctx, m); err != nil {
		t.Fatal(err)
	}
	other := m
	other.InternalClient = "10.0.0.1"
	if err := c.Add(ctx, other); !errors.Is(err, upnp.ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
	if got, err := c.Get(ctx, corev1.ProtocolTCP, 443); err != nil || got != m {
		t.Fatalf("got %+v %v", got, err)
	}
	if _, err := c.Get(ctx, corev1.ProtocolUDP, 443); !errors.Is(err, upnp.ErrNoSuchEntry) {
		t.Fatal(err)
	}
	if list, _ := c.List(ctx); len(list) != 1 {
		t.Fatalf("list %v", list)
	}
	if err := c.Delete(ctx, corev1.ProtocolTCP, 443); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, corev1.ProtocolTCP, 443); !errors.Is(err, upnp.ErrNoSuchEntry) {
		t.Fatal(err)
	}
	if c.Count("Add") != 2 || c.Count("Delete") != 2 {
		t.Fatalf("counts add=%d delete=%d", c.Count("Add"), c.Count("Delete"))
	}
}

func TestFake_InjectedErrorsAndStatus(t *testing.T) {
	c := fake.New()
	c.SetStatus(upnp.DeviceStatus{ExternalIP: "1.2.3.4"})
	c.SetTraffic(upnp.TrafficStats{BytesSent: 1})
	if st, _ := c.Status(ctx); st.ExternalIP != "1.2.3.4" {
		t.Fatal(st)
	}
	if tr, _ := c.Traffic(ctx); tr.BytesSent != 1 {
		t.Fatal(tr)
	}
	c.SetError("List", upnp.ErrUnreachable)
	if _, err := c.List(ctx); !errors.Is(err, upnp.ErrUnreachable) {
		t.Fatal(err)
	}
	c.SetError("List", nil)
	if _, err := c.List(ctx); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"Status", "Traffic", "Get", "Add", "Delete"} {
		c.SetError(method, upnp.ErrUnreachable)
	}
	if _, err := c.Status(ctx); err == nil {
		t.Fatal("Status")
	}
	if _, err := c.Traffic(ctx); err == nil {
		t.Fatal("Traffic")
	}
	if _, err := c.Get(ctx, corev1.ProtocolTCP, 1); err == nil {
		t.Fatal("Get")
	}
	if err := c.Add(ctx, upnp.PortMapping{}); err == nil {
		t.Fatal("Add")
	}
	if err := c.Delete(ctx, corev1.ProtocolTCP, 1); err == nil {
		t.Fatal("Delete")
	}
	c.Invalidate()
	if c.Count("Invalidate") != 1 {
		t.Fatal("Invalidate not counted")
	}
	c.ResetCalls()
	if c.Count("Status") != 0 {
		t.Fatal("not reset")
	}
}

func TestFake_SeedAndMappingsSorted(t *testing.T) {
	c := fake.New()
	c.Seed(
		upnp.PortMapping{Protocol: corev1.ProtocolUDP, ExternalPort: 1},
		upnp.PortMapping{Protocol: corev1.ProtocolTCP, ExternalPort: 9},
		upnp.PortMapping{Protocol: corev1.ProtocolTCP, ExternalPort: 2},
	)
	ms := c.Mappings()
	if len(ms) != 3 || ms[0].ExternalPort != 2 || ms[1].ExternalPort != 9 || ms[2].Protocol != corev1.ProtocolUDP {
		t.Fatalf("got %+v", ms)
	}
	c.Clear()
	if len(c.Mappings()) != 0 {
		t.Fatal("not cleared")
	}
}
