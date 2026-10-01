package fakeigd_test

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/huin/goupnp"
	"github.com/huin/goupnp/dcps/internetgateway2"
	"github.com/huin/goupnp/soap"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/nashant/upnp-nat-controller/internal/upnp/fakeigd"
)

var ctx = context.Background()

func ipConn(t *testing.T, f *fakeigd.Server) *internetgateway2.WANIPConnection2 {
	t.Helper()
	loc, _ := url.Parse(f.URL())
	cs, err := internetgateway2.NewWANIPConnection2ClientsByURLCtx(ctx, loc)
	if err != nil || len(cs) != 1 {
		t.Fatalf("clients: %v %v", cs, err)
	}
	return cs[0]
}

func faultCode(t *testing.T, err error) int {
	t.Helper()
	var f *soap.SOAPFaultError
	if !errors.As(err, &f) {
		t.Fatalf("want SOAP fault, got %v", err)
	}
	return f.Detail.UPnPError.Errorcode
}

func add(t *testing.T, c *internetgateway2.WANIPConnection2, port uint16, client, desc string, lease uint32) error {
	t.Helper()
	return c.AddPortMappingCtx(ctx, "", port, "TCP", port, client, true, desc, lease)
}

func TestFakeIGD_GenericEntry_713AtEnd(t *testing.T) {
	f := fakeigd.New(t)
	c := ipConn(t, f)
	if err := add(t, c, 443, "172.16.1.2", "a", 3600); err != nil {
		t.Fatal(err)
	}
	if err := add(t, c, 80, "172.16.1.2", "b", 3600); err != nil {
		t.Fatal(err)
	}
	_, p0, proto, ip, client, en, desc, lease, err := c.GetGenericPortMappingEntryCtx(ctx, 0)
	if err != nil || p0 != 80 || proto != "TCP" || ip != 80 || client != "172.16.1.2" || !en || desc != "b" || lease != 3600 {
		t.Fatalf("entry 0: %d %s %d %s %v %s %d %v", p0, proto, ip, client, en, desc, lease, err)
	}
	if _, p1, _, _, _, _, _, _, err := c.GetGenericPortMappingEntryCtx(ctx, 1); err != nil || p1 != 443 {
		t.Fatalf("entry 1: %d %v", p1, err)
	}
	_, _, _, _, _, _, _, _, err = c.GetGenericPortMappingEntryCtx(ctx, 2)
	if code := faultCode(t, err); code != 713 {
		t.Fatalf("want 713, got %d", code)
	}
}

func TestFakeIGD_Specific_714WhenAbsent(t *testing.T) {
	c := ipConn(t, fakeigd.New(t))
	_, _, _, _, _, err := c.GetSpecificPortMappingEntryCtx(ctx, "", 443, "TCP")
	if code := faultCode(t, err); code != 714 {
		t.Fatalf("want 714, got %d", code)
	}
	err = c.DeletePortMappingCtx(ctx, "", 443, "TCP")
	if code := faultCode(t, err); code != 714 {
		t.Fatalf("delete: want 714, got %d", code)
	}
}

func TestFakeIGD_AddSpecificDelete(t *testing.T) {
	f := fakeigd.New(t)
	c := ipConn(t, f)
	if err := add(t, c, 443, "172.16.1.2", "d", 3600); err != nil {
		t.Fatal(err)
	}
	ip, client, en, desc, lease, err := c.GetSpecificPortMappingEntryCtx(ctx, "", 443, "TCP")
	if err != nil || ip != 443 || client != "172.16.1.2" || !en || desc != "d" || lease != 3600 {
		t.Fatalf("got %d %s %v %s %d %v", ip, client, en, desc, lease, err)
	}
	if _, _, _, _, _, err := c.GetSpecificPortMappingEntryCtx(ctx, "", 443, "UDP"); faultCode(t, err) != 714 {
		t.Fatal("UDP/443 must be a separate key")
	}
	if err := c.DeletePortMappingCtx(ctx, "", 443, "TCP"); err != nil {
		t.Fatal(err)
	}
	if len(f.Mappings()) != 0 {
		t.Fatalf("table not empty: %v", f.Mappings())
	}
}

func TestFakeIGD_Add_718OnForeignConflict(t *testing.T) {
	f := fakeigd.New(t)
	c := ipConn(t, f)
	if err := add(t, c, 443, "192.168.1.50", "Xbox", 0); err != nil {
		t.Fatal(err)
	}
	if code := faultCode(t, add(t, c, 443, "172.16.1.2", "ours", 3600)); code != 718 {
		t.Fatalf("want 718, got %d", code)
	}
	// Same internal client overwrites (miniupnpd semantics).
	if err := add(t, c, 443, "192.168.1.50", "Xbox2", 0); err != nil {
		t.Fatal(err)
	}
	if m := f.Mappings(); len(m) != 1 || m[0].Description != "Xbox2" {
		t.Fatalf("got %+v", m)
	}
}

func TestFakeIGD_LeaseZeroStoredAs604800(t *testing.T) {
	c := ipConn(t, fakeigd.New(t))
	if err := add(t, c, 443, "172.16.1.2", "d", 0); err != nil {
		t.Fatal(err)
	}
	_, _, _, _, lease, err := c.GetSpecificPortMappingEntryCtx(ctx, "", 443, "TCP")
	if err != nil || lease != 604800 {
		t.Fatalf("lease %d, %v", lease, err)
	}
}

func TestFakeIGD_LeaseExpiry(t *testing.T) {
	clk := clocktesting.NewFakeClock(time.Unix(1_000_000, 0))
	f := fakeigd.New(t, fakeigd.WithClock(clk))
	c := ipConn(t, f)
	if err := add(t, c, 443, "172.16.1.2", "d", 100); err != nil {
		t.Fatal(err)
	}
	clk.Step(40 * time.Second)
	if _, _, _, _, lease, err := c.GetSpecificPortMappingEntryCtx(ctx, "", 443, "TCP"); err != nil || lease != 60 {
		t.Fatalf("remaining lease %d, %v; want 60", lease, err)
	}
	clk.Step(60 * time.Second)
	if _, _, _, _, _, err := c.GetSpecificPortMappingEntryCtx(ctx, "", 443, "TCP"); faultCode(t, err) != 714 {
		t.Fatal("expired entry still present")
	}
	if _, _, _, _, _, _, _, _, err := c.GetGenericPortMappingEntryCtx(ctx, 0); faultCode(t, err) != 713 {
		t.Fatal("expired entry still listed")
	}
}

func TestFakeIGD_StatusUptimeAndExternalIP(t *testing.T) {
	clk := clocktesting.NewFakeClock(time.Unix(1_000_000, 0))
	f := fakeigd.New(t, fakeigd.WithClock(clk))
	c := ipConn(t, f)
	clk.Step(90 * time.Second)
	st, _, up, err := c.GetStatusInfoCtx(ctx)
	if err != nil || st != "Connected" || up != 90 {
		t.Fatalf("status %q uptime %d %v", st, up, err)
	}
	f.SetExternalIP("81.2.69.160")
	f.SetConnectionStatus("Disconnected")
	if ip, err := c.GetExternalIPAddressCtx(ctx); err != nil || ip != "81.2.69.160" {
		t.Fatalf("ip %q %v", ip, err)
	}
	if st, _, _, _ := c.GetStatusInfoCtx(ctx); st != "Disconnected" {
		t.Fatalf("status %q", st)
	}
}

func TestFakeIGD_Traffic(t *testing.T) {
	f := fakeigd.New(t)
	f.SetTraffic(1000, 2000)
	loc, _ := url.Parse(f.URL())
	cs, err := internetgateway2.NewWANCommonInterfaceConfig1ClientsByURLCtx(ctx, loc)
	if err != nil {
		t.Fatal(err)
	}
	if s, err := cs[0].GetTotalBytesSentCtx(ctx); err != nil || s != 1000 {
		t.Fatalf("sent %d %v", s, err)
	}
	if r, err := cs[0].GetTotalBytesReceivedCtx(ctx); err != nil || r != 2000 {
		t.Fatalf("recv %d %v", r, err)
	}
}

func TestFakeIGD_RestartClearsAndMovesPort(t *testing.T) {
	clk := clocktesting.NewFakeClock(time.Unix(1_000_000, 0))
	f := fakeigd.New(t, fakeigd.WithClock(clk))
	c := ipConn(t, f)
	if err := add(t, c, 443, "172.16.1.2", "d", 3600); err != nil {
		t.Fatal(err)
	}
	clk.Step(time.Hour)
	oldURL := f.URL()
	f.RestartOnNewPort()
	if f.URL() == oldURL {
		t.Fatal("URL did not change")
	}
	if len(f.Mappings()) != 0 {
		t.Fatal("table not cleared")
	}
	if _, _, _, err := c.GetStatusInfoCtx(ctx); err == nil {
		t.Fatal("old client still works against old URL")
	}
	_, _, up, err := ipConn(t, f).GetStatusInfoCtx(ctx)
	if err != nil || up != 0 {
		t.Fatalf("uptime after restart %d, %v", up, err)
	}
}

func TestFakeIGD_RestartSamePort(t *testing.T) {
	f := fakeigd.New(t)
	c := ipConn(t, f)
	if err := add(t, c, 443, "172.16.1.2", "d", 3600); err != nil {
		t.Fatal(err)
	}
	u := f.URL()
	f.Restart()
	if f.URL() != u || len(f.Mappings()) != 0 {
		t.Fatalf("url %s->%s, mappings %v", u, f.URL(), f.Mappings())
	}
	if _, _, _, err := c.GetStatusInfoCtx(ctx); err != nil {
		t.Fatalf("same-port client broken: %v", err)
	}
}

func TestFakeIGD_StopStart(t *testing.T) {
	f := fakeigd.New(t)
	c := ipConn(t, f)
	f.Stop()
	if _, _, _, err := c.GetStatusInfoCtx(ctx); err == nil {
		t.Fatal("stopped server answered")
	}
	f.Start()
	if _, _, _, err := c.GetStatusInfoCtx(ctx); err != nil {
		t.Fatalf("after start: %v", err)
	}
}

func TestFakeIGD_StartedStopped(t *testing.T) {
	f := fakeigd.New(t, fakeigd.Stopped())
	loc, _ := url.Parse(f.URL())
	if _, err := internetgateway2.NewWANIPConnection2ClientsByURLCtx(ctx, loc); err == nil {
		t.Fatal("stopped server answered")
	}
	f.Start()
	ipConn(t, f)
}

func TestFakeIGD_ForcedFault(t *testing.T) {
	f := fakeigd.New(t)
	c := ipConn(t, f)
	f.SetFault("AddPortMapping", 501)
	if code := faultCode(t, add(t, c, 443, "172.16.1.2", "d", 3600)); code != 501 {
		t.Fatalf("got %d", code)
	}
	f.SetFault("AddPortMapping", 0)
	if err := add(t, c, 443, "172.16.1.2", "d", 3600); err != nil {
		t.Fatal(err)
	}
}

func TestFakeIGD_OnlyPermanentLeases(t *testing.T) {
	f := fakeigd.New(t, fakeigd.OnlyPermanentLeases())
	c := ipConn(t, f)
	if code := faultCode(t, add(t, c, 443, "172.16.1.2", "d", 3600)); code != 725 {
		t.Fatalf("got %d", code)
	}
	if err := add(t, c, 443, "172.16.1.2", "d", 0); err != nil {
		t.Fatal(err)
	}
}

func TestFakeIGD_Hang(t *testing.T) {
	f := fakeigd.New(t)
	c := ipConn(t, f)
	f.SetHang(true)
	tctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, _, _, err := c.GetStatusInfoCtx(tctx); err == nil {
		t.Fatal("hung server answered")
	}
	f.SetHang(false)
	if _, _, _, err := c.GetStatusInfoCtx(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestFakeIGD_RequestLog(t *testing.T) {
	f := fakeigd.New(t)
	c := ipConn(t, f)
	_ = add(t, c, 443, "172.16.1.2", "d", 3600)
	_, _, _, _ = c.GetStatusInfoCtx(ctx)
	if got := f.Count("AddPortMapping"); got != 1 {
		t.Fatalf("AddPortMapping count %d", got)
	}
	reqs := f.Requests()
	if len(reqs) != 2 || reqs[0].Action != "AddPortMapping" || reqs[0].Args["NewExternalPort"] != "443" {
		t.Fatalf("got %+v", reqs)
	}
	f.ResetRequests()
	if len(f.Requests()) != 0 {
		t.Fatal("not reset")
	}
}

func TestFakeIGD_IGDv1AndV2Descriptions(t *testing.T) {
	for _, v := range []int{1, 2} {
		f := fakeigd.New(t, fakeigd.WithDeviceVersion(v), fakeigd.WithServices(fakeigd.WANIPConnection1))
		loc, _ := url.Parse(f.URL())
		root, err := goupnp.DeviceByURLCtx(ctx, loc)
		if err != nil {
			t.Fatal(err)
		}
		wantType := map[int]string{1: "urn:schemas-upnp-org:device:InternetGatewayDevice:1", 2: "urn:schemas-upnp-org:device:InternetGatewayDevice:2"}[v]
		if root.Device.DeviceType != wantType {
			t.Errorf("v%d: device type %q", v, root.Device.DeviceType)
		}
		cs, err := internetgateway2.NewWANIPConnection1ClientsByURLCtx(ctx, loc)
		if err != nil || len(cs) != 1 {
			t.Fatalf("v%d: %v %v", v, cs, err)
		}
		if _, err := cs[0].GetExternalIPAddressCtx(ctx); err != nil {
			t.Fatalf("v%d: %v", v, err)
		}
		if _, err := internetgateway2.NewWANIPConnection2ClientsByURLCtx(ctx, loc); err == nil {
			t.Errorf("v%d: WANIPConnection2 should not be advertised", v)
		}
	}
}

func TestFakeIGD_MultipleServicesShareTable(t *testing.T) {
	f := fakeigd.New(t, fakeigd.WithServices(fakeigd.WANPPPConnection1, fakeigd.WANIPConnection1))
	loc, _ := url.Parse(f.URL())
	ppp, err := internetgateway2.NewWANPPPConnection1ClientsByURLCtx(ctx, loc)
	if err != nil {
		t.Fatal(err)
	}
	if err := ppp[0].AddPortMappingCtx(ctx, "", 443, "TCP", 443, "172.16.1.2", true, "d", 0); err != nil {
		t.Fatal(err)
	}
	ip1, err := internetgateway2.NewWANIPConnection1ClientsByURLCtx(ctx, loc)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, _, err := ip1[0].GetSpecificPortMappingEntryCtx(ctx, "", 443, "TCP"); err != nil {
		t.Fatal(err)
	}
}

func TestFakeIGD_ControlURLHostOverride(t *testing.T) {
	f := fakeigd.New(t, fakeigd.WithControlHost("10.99.99.99:1"))
	loc, _ := url.Parse(f.URL())
	cs, err := internetgateway2.NewWANIPConnection2ClientsByURLCtx(ctx, loc)
	if err != nil {
		t.Fatal(err)
	}
	if h := cs[0].SOAPClient.EndpointURL.Host; h != "10.99.99.99:1" {
		t.Fatalf("control host %q", h)
	}
}

func TestFakeIGD_WithoutTrafficCounters(t *testing.T) {
	f := fakeigd.New(t, fakeigd.WithoutTrafficCounters())
	loc, _ := url.Parse(f.URL())
	if _, err := internetgateway2.NewWANCommonInterfaceConfig1ClientsByURLCtx(ctx, loc); err == nil {
		t.Fatal("WANCommonInterfaceConfig advertised")
	}
}
