package controller

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	evts "k8s.io/client-go/tools/events"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gatewayv1alpha1 "github.com/nashant/upnp-nat-controller/api/v1alpha1"
	"github.com/nashant/upnp-nat-controller/internal/mapping"
	"github.com/nashant/upnp-nat-controller/internal/metrics"
	"github.com/nashant/upnp-nat-controller/internal/upnp"
	"github.com/nashant/upnp-nat-controller/internal/upnp/fake"
)

type countingTrigger struct{ n atomic.Int32 }

func (c *countingTrigger) TriggerResync() { c.n.Add(1) }

type igdHarness struct {
	p       *IGDPoller
	router  *fake.Client
	rec     *evts.FakeRecorder
	clk     *clocktesting.FakeClock
	trigger *countingTrigger
	m       *metrics.Metrics
}

func newIGDHarness(t *testing.T) *igdHarness {
	t.Helper()
	cleanupIGD(t)
	t.Cleanup(func() { cleanupIGD(t) })
	h := &igdHarness{
		router:  fake.New(),
		rec:     evts.NewFakeRecorder(100),
		clk:     clocktesting.NewFakeClock(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)),
		trigger: &countingTrigger{},
		m:       metrics.New(prometheus.NewRegistry()),
	}
	h.p = &IGDPoller{K8s: k8s, UPnP: h.router, Recorder: h.rec, Clock: h.clk, Resync: h.trigger, Metrics: h.m}
	return h
}

func cleanupIGD(t *testing.T) {
	t.Helper()
	err := k8s.Delete(bg, &gatewayv1alpha1.InternetGatewayDevice{ObjectMeta: metav1.ObjectMeta{Name: IGDName}})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
}

func (h *igdHarness) poll(t *testing.T) time.Duration {
	t.Helper()
	d, err := h.p.Poll(bg)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	return d
}

func getIGD(t *testing.T) *gatewayv1alpha1.InternetGatewayDevice {
	t.Helper()
	var igd gatewayv1alpha1.InternetGatewayDevice
	if err := k8s.Get(bg, client.ObjectKey{Name: IGDName}, &igd); err != nil {
		t.Fatal(err)
	}
	return &igd
}

func condStatus(igd *gatewayv1alpha1.InternetGatewayDevice, typ string) metav1.ConditionStatus {
	c := meta.FindStatusCondition(igd.Status.Conditions, typ)
	if c == nil {
		return "missing"
	}
	return c.Status
}

func drainEvents(rec *evts.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func hasEvent(evs []string, reason string) bool {
	return countEvents(evs, reason) > 0
}

func countEvents(evs []string, reason string) int {
	n := 0
	for _, e := range evs {
		if strings.Contains(e, " "+reason+" ") {
			n++
		}
	}
	return n
}

func TestIGD_CreatesDefaultCR(t *testing.T) {
	h := newIGDHarness(t)
	if d := h.poll(t); d != 30*time.Second {
		t.Fatalf("next poll in %v, want 30s default", d)
	}
	igd := getIGD(t)
	if igd.Spec.PollingInterval != 30 {
		t.Fatalf("pollingInterval %d", igd.Spec.PollingInterval)
	}
}

func TestIGD_UsesSpecPollingInterval(t *testing.T) {
	h := newIGDHarness(t)
	h.poll(t)
	igd := getIGD(t)
	igd.Spec.PollingInterval = 45
	if err := k8s.Update(bg, igd); err != nil {
		t.Fatal(err)
	}
	if d := h.poll(t); d != 45*time.Second {
		t.Fatalf("next poll %v, want 45s", d)
	}
}

func TestIGD_StatusFieldsPopulated(t *testing.T) {
	h := newIGDHarness(t)
	owned := upnp.PortMapping{Protocol: corev1.ProtocolTCP, ExternalPort: 443, InternalPort: 443, InternalClient: "172.16.1.2", Enabled: true,
		Description: mapping.DefaultDescriptions.For(mapping.Owner{Namespace: "traefik", Name: "public-traefik"}), LeaseDuration: 3000}
	foreign := upnp.PortMapping{Protocol: corev1.ProtocolTCP, ExternalPort: 80, InternalPort: 80, InternalClient: "192.168.1.50", Enabled: true, Description: "Xbox"}
	h.router.Seed(owned, foreign)
	h.poll(t)
	st := getIGD(t).Status
	if st.FriendlyName != "Fake IGD" || st.Location != "http://192.168.1.1:5000/rootDesc.xml" || st.InternalIP != "192.168.1.1" ||
		st.ExternalIP != "81.2.69.142" || st.ConnectionStatus != "Connected" || st.Uptime != 1000 {
		t.Fatalf("status %+v", st)
	}
	if st.LastSeen == nil || !st.LastSeen.Time.Equal(h.clk.Now()) {
		t.Fatalf("lastSeen %v, want %v", st.LastSeen, h.clk.Now())
	}
	want := []gatewayv1alpha1.PortMappingStatus{{
		Protocol: "TCP", ExternalPort: 443, InternalPort: 443, InternalClient: "172.16.1.2", Enabled: true,
		Description: owned.Description, LeaseDuration: 3000,
		ServiceRef: gatewayv1alpha1.ServiceRef{Namespace: "traefik", Name: "public-traefik"},
	}}
	if len(st.PortMappings) != 1 || st.PortMappings[0] != want[0] {
		t.Fatalf("portMappings %+v, want only owned %+v", st.PortMappings, want)
	}
	if v := testutil.ToFloat64(h.m.OwnedMappings); v != 1 {
		t.Fatalf("owned mappings metric %v", v)
	}
	if v := testutil.ToFloat64(h.m.Uptime); v != 1000 {
		t.Fatalf("uptime metric %v", v)
	}
	if v := testutil.ToFloat64(h.m.ExternalIPInfo.WithLabelValues("81.2.69.142")); v != 1 {
		t.Fatalf("external ip info %v", v)
	}
}

func TestIGD_ConditionsTransition_DiscoveredReachableConnectedReady(t *testing.T) {
	h := newIGDHarness(t)
	h.router.SetError("Status", upnp.ErrNoDevice)
	h.poll(t)
	igd := getIGD(t)
	for _, c := range []string{gatewayv1alpha1.ConditionDiscovered, gatewayv1alpha1.ConditionReachable, gatewayv1alpha1.ConditionReady} {
		if s := condStatus(igd, c); s != metav1.ConditionFalse {
			t.Errorf("no device: %s=%s, want False", c, s)
		}
	}
	if v := testutil.ToFloat64(h.m.Discovered); v != 0 {
		t.Errorf("discovered metric %v", v)
	}

	h.router.SetError("Status", nil)
	h.clk.Step(30 * time.Second)
	h.poll(t)
	igd = getIGD(t)
	for _, c := range []string{gatewayv1alpha1.ConditionDiscovered, gatewayv1alpha1.ConditionReachable, gatewayv1alpha1.ConditionConnected, gatewayv1alpha1.ConditionPublicExternalIP, gatewayv1alpha1.ConditionReady} {
		if s := condStatus(igd, c); s != metav1.ConditionTrue {
			t.Errorf("healthy: %s=%s, want True", c, s)
		}
	}
	if v := testutil.ToFloat64(h.m.Discovered); v != 1 {
		t.Errorf("discovered metric %v", v)
	}

	h.router.UpdateStatus(func(s *upnp.DeviceStatus) { s.ConnectionStatus = "Disconnected" })
	h.clk.Step(30 * time.Second)
	h.poll(t)
	igd = getIGD(t)
	if condStatus(igd, gatewayv1alpha1.ConditionConnected) != metav1.ConditionFalse || condStatus(igd, gatewayv1alpha1.ConditionReady) != metav1.ConditionFalse {
		t.Errorf("disconnected: Connected=%s Ready=%s", condStatus(igd, gatewayv1alpha1.ConditionConnected), condStatus(igd, gatewayv1alpha1.ConditionReady))
	}

	h.router.SetError("Status", upnp.ErrUnreachable)
	h.clk.Step(30 * time.Second)
	h.poll(t)
	igd = getIGD(t)
	if condStatus(igd, gatewayv1alpha1.ConditionDiscovered) != metav1.ConditionTrue || condStatus(igd, gatewayv1alpha1.ConditionReachable) != metav1.ConditionFalse {
		t.Errorf("unreachable after discovery: Discovered=%s Reachable=%s", condStatus(igd, gatewayv1alpha1.ConditionDiscovered), condStatus(igd, gatewayv1alpha1.ConditionReachable))
	}
}

func TestIGD_DoubleNAT_PublicExternalIPFalse(t *testing.T) {
	h := newIGDHarness(t)
	h.router.UpdateStatus(func(s *upnp.DeviceStatus) { s.ExternalIP = "192.168.1.10" })
	h.poll(t)
	c := meta.FindStatusCondition(getIGD(t).Status.Conditions, gatewayv1alpha1.ConditionPublicExternalIP)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "Private" || !strings.Contains(c.Message, "192.168.1.10") {
		t.Fatalf("got %+v", c)
	}
	if condStatus(getIGD(t), gatewayv1alpha1.ConditionReady) != metav1.ConditionTrue {
		t.Fatal("double NAT must not make the controller unready")
	}
}

func TestIGD_UptimeDecrease_EmitsRouterRestarted_TriggersResync(t *testing.T) {
	h := newIGDHarness(t)
	h.poll(t)
	drainEvents(h.rec)
	h.router.UpdateStatus(func(s *upnp.DeviceStatus) { s.Uptime = 1030 })
	h.clk.Step(30 * time.Second)
	h.poll(t)
	if n := h.trigger.n.Load(); n != 0 {
		t.Fatalf("resync on normal poll: %d", n)
	}
	h.router.UpdateStatus(func(s *upnp.DeviceStatus) { s.Uptime = 5 })
	h.clk.Step(30 * time.Second)
	h.poll(t)
	if !hasEvent(drainEvents(h.rec), "RouterRestarted") {
		t.Fatal("no RouterRestarted event")
	}
	if n := h.trigger.n.Load(); n != 1 {
		t.Fatalf("resyncs %d, want 1", n)
	}
	if v := testutil.ToFloat64(h.m.RouterRestarts); v != 1 {
		t.Fatalf("router restarts metric %v", v)
	}
}

func TestIGD_LocationChange_EmitsRouterRestarted(t *testing.T) {
	h := newIGDHarness(t)
	h.poll(t)
	drainEvents(h.rec)
	h.router.UpdateStatus(func(s *upnp.DeviceStatus) { s.Location = "http://192.168.1.1:5001/rootDesc.xml"; s.Uptime = 2000 })
	h.clk.Step(30 * time.Second)
	h.poll(t)
	if !hasEvent(drainEvents(h.rec), "RouterRestarted") || h.trigger.n.Load() != 1 {
		t.Fatal("location change not detected")
	}
}

func TestIGD_ExternalIPChange_EmitsEvent_TriggersResync(t *testing.T) {
	h := newIGDHarness(t)
	h.poll(t)
	drainEvents(h.rec)
	h.router.UpdateStatus(func(s *upnp.DeviceStatus) { s.ExternalIP = "81.2.69.200"; s.Uptime = 1030 })
	h.clk.Step(30 * time.Second)
	h.poll(t)
	evs := drainEvents(h.rec)
	if !hasEvent(evs, "ExternalIPChanged") || hasEvent(evs, "RouterRestarted") {
		t.Fatalf("events %v", evs)
	}
	if h.trigger.n.Load() != 1 {
		t.Fatal("no resync")
	}
	if getIGD(t).Status.ExternalIP != "81.2.69.200" {
		t.Fatal("status not updated")
	}
	if v := testutil.ToFloat64(h.m.ExternalIPInfo.WithLabelValues("81.2.69.142")); v != 0 {
		t.Fatal("old external IP series not removed")
	}
}

func TestIGD_RestartDetectedAcrossControllerRestart(t *testing.T) { // previous state seeded from CR status
	h := newIGDHarness(t)
	h.poll(t)
	h2 := &IGDPoller{K8s: k8s, UPnP: h.router, Recorder: h.rec, Clock: h.clk, Resync: h.trigger, Metrics: h.m}
	h.router.UpdateStatus(func(s *upnp.DeviceStatus) { s.Uptime = 3 })
	if _, err := h2.Poll(bg); err != nil {
		t.Fatal(err)
	}
	if !hasEvent(drainEvents(h.rec), "RouterRestarted") {
		t.Fatal("restart during controller downtime not detected")
	}
}

func TestIGD_BecomesReachable_TriggersResync(t *testing.T) {
	h := newIGDHarness(t)
	h.router.SetError("Status", upnp.ErrNoDevice)
	h.poll(t)
	h.router.SetError("Status", nil)
	h.clk.Step(30 * time.Second)
	h.poll(t)
	if h.trigger.n.Load() != 1 {
		t.Fatalf("resyncs %d, want 1 on becoming reachable", h.trigger.n.Load())
	}
}

func TestIGD_StatusNotRewrittenWhenUnchanged(t *testing.T) {
	h := newIGDHarness(t)
	h.router.Seed(upnp.PortMapping{Protocol: corev1.ProtocolTCP, ExternalPort: 443, InternalPort: 443, InternalClient: "172.16.1.2", Enabled: true,
		Description: mapping.DefaultDescriptions.For(mapping.Owner{Namespace: "a", Name: "b"}), LeaseDuration: 3000})
	h.poll(t)
	rv := getIGD(t).ResourceVersion
	for i := 0; i < 5; i++ { // uptime, lease and lastSeen tick; nothing else changes
		h.clk.Step(30 * time.Second)
		h.router.UpdateStatus(func(s *upnp.DeviceStatus) { s.Uptime += 30 })
		h.poll(t)
	}
	if got := getIGD(t).ResourceVersion; got != rv {
		t.Fatalf("status rewritten without change: rv %s -> %s", rv, got)
	}
	h.clk.Step(3 * time.Minute) // lastSeen now > 5m old
	h.poll(t)
	igd := getIGD(t)
	if igd.ResourceVersion == rv || !igd.Status.LastSeen.Time.Equal(h.clk.Now()) {
		t.Fatal("stale lastSeen not refreshed")
	}
	rv = igd.ResourceVersion
	h.router.UpdateStatus(func(s *upnp.DeviceStatus) { s.ConnectionStatus = "Connecting" })
	h.clk.Step(30 * time.Second)
	h.poll(t)
	if getIGD(t).ResourceVersion == rv {
		t.Fatal("real change not written")
	}
}

func TestIGD_TrafficOnlyInMetrics(t *testing.T) {
	h := newIGDHarness(t)
	h.router.SetTraffic(upnp.TrafficStats{BytesSent: 100, BytesReceived: 200})
	h.poll(t)
	rv := getIGD(t).ResourceVersion
	if v := testutil.ToFloat64(h.m.TrafficBytes.WithLabelValues("sent")); v != 100 {
		t.Fatalf("sent %v", v)
	}
	if v := testutil.ToFloat64(h.m.TrafficBytes.WithLabelValues("received")); v != 200 {
		t.Fatalf("received %v", v)
	}
	h.router.SetTraffic(upnp.TrafficStats{BytesSent: 999, BytesReceived: 999})
	h.clk.Step(30 * time.Second)
	h.poll(t)
	if getIGD(t).ResourceVersion != rv {
		t.Fatal("traffic change caused a status write")
	}
}

func TestIGD_ListFailureKeepsPortMappings(t *testing.T) {
	h := newIGDHarness(t)
	h.router.Seed(upnp.PortMapping{Protocol: corev1.ProtocolTCP, ExternalPort: 443, InternalPort: 443, InternalClient: "172.16.1.2", Enabled: true,
		Description: mapping.DefaultDescriptions.For(mapping.Owner{Namespace: "a", Name: "b"})})
	h.poll(t)
	h.router.SetError("List", upnp.ErrUnreachable)
	h.clk.Step(30 * time.Second)
	h.poll(t)
	if n := len(getIGD(t).Status.PortMappings); n != 1 {
		t.Fatalf("portMappings %d after failed List, want 1", n)
	}
}

func TestIGD_Start_PollsOnIntervalAndReportsPass(t *testing.T) {
	h := newIGDHarness(t)
	var passes atomic.Int32
	h.p.OnPass = func(next time.Duration) {
		if next != 30*time.Second {
			t.Errorf("OnPass next=%v", next)
		}
		passes.Add(1)
	}
	ctx, cancel := context.WithCancel(bg)
	done := make(chan error)
	go func() { done <- h.p.Start(ctx) }()
	waitFor := func(n int32) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for passes.Load() < n {
			if time.Now().After(deadline) {
				t.Fatalf("passes %d, want %d", passes.Load(), n)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitFor(1)
	for !h.clk.HasWaiters() {
		time.Sleep(time.Millisecond)
	}
	h.clk.Step(30 * time.Second)
	waitFor(2)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
