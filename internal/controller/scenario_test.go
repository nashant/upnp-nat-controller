package controller

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"golang.org/x/time/rate"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/clock"
	clocktesting "k8s.io/utils/clock/testing"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	gatewayv1alpha1 "github.com/nashant/upnp-nat-controller/api/v1alpha1"
	"github.com/nashant/upnp-nat-controller/internal/annotations"
	"github.com/nashant/upnp-nat-controller/internal/mapping"
	"github.com/nashant/upnp-nat-controller/internal/metrics"
	"github.com/nashant/upnp-nat-controller/internal/upnp"
	"github.com/nashant/upnp-nat-controller/internal/upnp/fakeigd"
)

// collectingRecorder is a thread-safe EventRecorder that never blocks.
type collectingRecorder struct {
	mu     sync.Mutex
	events []string
}

func (c *collectingRecorder) Eventf(obj, _ runtime.Object, typ, reason, _, note string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	o, _ := meta.Accessor(obj)
	c.events = append(c.events, fmt.Sprintf("%s %s %s/%s %s", typ, reason, o.GetNamespace(), o.GetName(), fmt.Sprintf(note, args...)))
}

func (c *collectingRecorder) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.events...)
}

// permanentLeases suits scenarios with a resync interval longer than any lease.
func permanentLeases(r *ServiceReconciler) { r.DefaultLease = 0 }

func (c *collectingRecorder) has(reason string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.events {
		if strings.Contains(e, " "+reason+" ") {
			return true
		}
	}
	return false
}

type scenario struct {
	t      *testing.T
	ns     string
	f      *fakeigd.Server
	rec    *collectingRecorder
	m      *metrics.Metrics
	client upnp.Client
	poller *IGDPoller
}

type scenarioOpts struct {
	fake   []fakeigd.Option
	resync time.Duration
	tweak  func(*ServiceReconciler)
}

func startScenario(t *testing.T, o scenarioOpts) *scenario {
	t.Helper()
	if o.resync == 0 {
		o.resync = 300 * time.Millisecond
	}
	s := &scenario{t: t, ns: newNamespace(t), rec: &collectingRecorder{}, m: metrics.New(prometheus.NewRegistry())}
	s.f = fakeigd.New(t, o.fake...)
	s.client = metrics.Instrument(upnp.New(upnp.Config{
		Searcher: upnp.SearcherFunc(func(context.Context) ([]*url.URL, error) {
			u, err := url.Parse(s.f.URL())
			return []*url.URL{u}, err
		}),
		SOAPTimeout: time.Second,
		Limiter:     rate.NewLimiter(rate.Inf, 0),
		Backoff:     &upnp.Backoff{Initial: 50 * time.Millisecond, Max: 200 * time.Millisecond},
	}), s.m)

	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Cache:                  cache.Options{DefaultNamespaces: map[string]cache.Config{s.ns: {}}},
		Controller:             config.Controller{SkipNameValidation: ptr.To(true)},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := &ServiceReconciler{
		K8s: mgr.GetClient(), UPnP: s.client, Recorder: s.rec, Clock: clock.RealClock{}, Metrics: s.m,
		ResyncInterval: o.resync, RetryInterval: 100 * time.Millisecond, IPWaitInterval: 100 * time.Millisecond,
		DefaultLease: 3600, FinalizerTimeout: time.Second,
	}
	if o.tweak != nil {
		o.tweak(r)
	}
	resync := NewResync(mgr.GetClient())
	if err := r.SetupWithManager(mgr, controller.Options{}, resync.Source()); err != nil {
		t.Fatal(err)
	}
	cleanupIGD(t)
	s.poller = &IGDPoller{K8s: k8s, UPnP: s.client, Recorder: s.rec, Clock: clock.RealClock{}, Resync: resync, Metrics: s.m}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := mgr.Start(ctx); err != nil {
			t.Errorf("manager: %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		cleanupIGD(t)
	})
	return s
}

func (s *scenario) eventually(what string, cond func() bool) {
	s.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			s.t.Fatalf("timed out waiting for %s; router: %+v; events: %v", what, s.f.Mappings(), s.rec.snapshot())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (s *scenario) poll() {
	s.t.Helper()
	if _, err := s.poller.Poll(bg); err != nil {
		s.t.Fatal(err)
	}
}

func (s *scenario) ports(desc string) []string {
	var out []string
	for _, m := range s.f.Mappings() {
		if desc == "" || m.Description == desc {
			out = append(out, fmt.Sprintf("%s/%d->%s:%d", m.Protocol, m.ExternalPort, m.InternalClient, m.InternalPort))
		}
	}
	return out
}

func (s *scenario) hasPorts(desc string, want ...string) func() bool {
	return func() bool {
		got := s.ports(desc)
		if len(got) != len(want) {
			return false
		}
		for i := range want {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}
}

func (s *scenario) mappedLB(name string) (*corev1.Service, string) {
	s.t.Helper()
	svc := managedLB(s.t, s.ns, name, lbIP)
	desc := mapping.DefaultDescriptions.For(mapping.Owner{Namespace: s.ns, Name: name})
	s.eventually("initial mappings", s.hasPorts(desc, "TCP/443->"+lbIP+":443", "TCP/32400->"+lbIP+":32400"))
	return svc, desc
}

func gone(svc *corev1.Service) func() bool {
	return func() bool {
		return apierrors.IsNotFound(k8s.Get(bg, client.ObjectKeyFromObject(svc), &corev1.Service{}))
	}
}

func TestScenario_RouterReboot(t *testing.T) {
	s := startScenario(t, scenarioOpts{})
	_, desc := s.mappedLB("lb")
	s.f.Restart()
	s.eventually("mappings restored", s.hasPorts(desc, "TCP/443->"+lbIP+":443", "TCP/32400->"+lbIP+":32400"))
	if v := testutil.ToFloat64(s.m.DriftRepairs); v < 1 {
		t.Fatalf("drift repairs %v", v)
	}
}

func TestScenario_RouterReboot_NewPort(t *testing.T) {
	s := startScenario(t, scenarioOpts{})
	_, desc := s.mappedLB("lb")
	old := s.f.URL()
	s.f.RestartOnNewPort()
	if s.f.URL() == old {
		t.Fatal("port did not change")
	}
	s.eventually("mappings restored on new port", s.hasPorts(desc, "TCP/443->"+lbIP+":443", "TCP/32400->"+lbIP+":32400"))
}

func TestScenario_RouterDownAtBoot(t *testing.T) {
	s := startScenario(t, scenarioOpts{fake: []fakeigd.Option{fakeigd.Stopped()}})
	svc := managedLB(t, s.ns, "lb", lbIP)
	s.eventually("unreachable reconciles", func() bool {
		return testutil.ToFloat64(s.m.ReconcileTotal.WithLabelValues("router_unreachable")) >= 2
	})
	s.poll()
	if c := meta.FindStatusCondition(getIGD(t).Status.Conditions, gatewayv1alpha1.ConditionDiscovered); c == nil || c.Status != metav1.ConditionFalse {
		t.Fatalf("Discovered %+v, want False", c)
	}
	s.f.Start()
	s.eventually("mapped after router start", s.hasPorts(mapping.DefaultDescriptions.For(mapping.Owner{Namespace: s.ns, Name: svc.Name}), "TCP/443->"+lbIP+":443", "TCP/32400->"+lbIP+":32400"))
	s.eventually("Discovered=True", func() bool {
		s.poll()
		c := meta.FindStatusCondition(getIGD(t).Status.Conditions, gatewayv1alpha1.ConditionDiscovered)
		return c != nil && c.Status == metav1.ConditionTrue
	})
}

func TestScenario_LeaseExpiry(t *testing.T) {
	clk := clocktesting.NewFakeClock(time.Unix(1_000_000, 0))
	s := startScenario(t, scenarioOpts{fake: []fakeigd.Option{fakeigd.WithClock(clk)}, resync: time.Second})
	svc := createService(t, s.ns, "lb", corev1.ServiceTypeLoadBalancer,
		map[string]string{annotations.TCPEnabled: "true", annotations.TCPPorts: "443", annotations.LeaseSeconds: "10"}, tcpPort(443))
	setIngress(t, svc, corev1.LoadBalancerIngress{IP: lbIP})
	s.eventually("mapped", func() bool { return len(s.f.Mappings()) == 1 })
	adds := s.f.Count("AddPortMapping")
	clk.Step(9 * time.Second) // 1s left, below 2 × resync interval
	s.eventually("renewed", func() bool {
		ms := s.f.Mappings()
		return len(ms) == 1 && ms[0].Lease == 10 && s.f.Count("AddPortMapping") > adds
	})
}

func TestScenario_MultipleServices(t *testing.T) {
	s := startScenario(t, scenarioOpts{})
	_, descA := s.mappedLB("a")
	b := createService(t, s.ns, "b", corev1.ServiceTypeLoadBalancer,
		map[string]string{annotations.TCPEnabled: "true", annotations.TCPPorts: "8443"}, tcpPort(8443))
	setIngress(t, b, corev1.LoadBalancerIngress{IP: "172.16.1.3"})
	descB := mapping.DefaultDescriptions.For(mapping.Owner{Namespace: s.ns, Name: "b"})
	s.eventually("b mapped", s.hasPorts(descB, "TCP/8443->172.16.1.3:8443"))
	s.f.Restart()
	s.eventually("a restored", s.hasPorts(descA, "TCP/443->"+lbIP+":443", "TCP/32400->"+lbIP+":32400"))
	s.eventually("b restored", s.hasPorts(descB, "TCP/8443->172.16.1.3:8443"))
}

func TestScenario_UDPOnly(t *testing.T) {
	s := startScenario(t, scenarioOpts{})
	svc := createService(t, s.ns, "wg", corev1.ServiceTypeLoadBalancer,
		map[string]string{annotations.UDPEnabled: "true", annotations.UDPPorts: "51820"}, udpPort(51820))
	setIngress(t, svc, corev1.LoadBalancerIngress{IP: lbIP})
	s.eventually("UDP mapped", s.hasPorts(mapping.DefaultDescriptions.For(mapping.Owner{Namespace: s.ns, Name: "wg"}), "UDP/51820->"+lbIP+":51820"))
}

func TestScenario_DeleteService(t *testing.T) {
	s := startScenario(t, scenarioOpts{})
	svc, _ := s.mappedLB("lb")
	if err := k8s.Delete(bg, svc); err != nil {
		t.Fatal(err)
	}
	s.eventually("Service gone", gone(svc))
	if len(s.f.Mappings()) != 0 {
		t.Fatalf("mappings left: %v", s.ports(""))
	}
}

func TestScenario_DeleteWhileRouterDown(t *testing.T) {
	s := startScenario(t, scenarioOpts{})
	svc, _ := s.mappedLB("lb")
	s.f.Stop()
	if err := k8s.Delete(bg, svc); err != nil {
		t.Fatal(err)
	}
	s.eventually("Service gone after finalizer timeout", gone(svc))
	if !s.rec.has("OrphanedMappings") {
		t.Fatal("no OrphanedMappings event")
	}
}

func TestScenario_Conflict(t *testing.T) {
	s := startScenario(t, scenarioOpts{})
	foreign := fakeigd.Mapping{Protocol: "TCP", ExternalPort: 443, InternalPort: 443, InternalClient: "192.168.1.50", Enabled: true, Description: "Xbox"}
	s.f.AddMapping(foreign)
	managedLB(t, s.ns, "lb", lbIP)
	s.eventually("other port mapped", s.hasPorts(mapping.DefaultDescriptions.For(mapping.Owner{Namespace: s.ns, Name: "lb"}), "TCP/32400->"+lbIP+":32400"))
	s.eventually("PortConflict event", func() bool { return s.rec.has("PortConflict") })
	if got := s.ports("Xbox"); len(got) != 1 || got[0] != "TCP/443->192.168.1.50:443" {
		t.Fatalf("foreign mapping changed: %v", got)
	}
}

func TestScenario_LBIPChange(t *testing.T) {
	s := startScenario(t, scenarioOpts{})
	svc, desc := s.mappedLB("lb")
	setIngress(t, svc, corev1.LoadBalancerIngress{IP: "172.16.1.9"})
	s.eventually("moved to new IP", s.hasPorts(desc, "TCP/443->172.16.1.9:443", "TCP/32400->172.16.1.9:32400"))
}

func TestScenario_AnnotationRemoved(t *testing.T) {
	s := startScenario(t, scenarioOpts{})
	svc, _ := s.mappedLB("lb")
	svc = getSvc(t, svc)
	delete(svc.Annotations, annotations.TCPEnabled)
	if err := k8s.Update(bg, svc); err != nil {
		t.Fatal(err)
	}
	s.eventually("cleaned up", func() bool {
		return len(s.f.Mappings()) == 0 && !controllerutil.ContainsFinalizer(getSvc(t, svc), Finalizer)
	})
}

func TestScenario_PortListChanged(t *testing.T) {
	s := startScenario(t, scenarioOpts{})
	svc, desc := s.mappedLB("lb")
	s.f.ResetRequests()
	svc = getSvc(t, svc)
	svc.Annotations[annotations.TCPPorts] = "443"
	if err := k8s.Update(bg, svc); err != nil {
		t.Fatal(err)
	}
	s.eventually("32400 removed", s.hasPorts(desc, "TCP/443->"+lbIP+":443"))
	if n := s.f.Count("AddPortMapping"); n != 0 {
		t.Fatalf("443 churned: %d adds", n)
	}
	if n := s.f.Count("DeletePortMapping"); n != 1 {
		t.Fatalf("deletes %d, want 1", n)
	}
}

func TestScenario_UnsupportedServiceType(t *testing.T) {
	s := startScenario(t, scenarioOpts{})
	createService(t, s.ns, "plex", corev1.ServiceTypeClusterIP,
		map[string]string{annotations.TCPEnabled: "true", annotations.TCPPorts: "32400"}, tcpPort(32400))
	s.eventually("UnsupportedServiceType event", func() bool { return s.rec.has("UnsupportedServiceType") })
	if n := len(s.f.Requests()); n != 0 {
		t.Fatalf("router saw %d requests", n)
	}
}

func TestScenario_ExternalIPChanged(t *testing.T) {
	// A long resync interval proves the restore comes from the triggered resync.
	s := startScenario(t, scenarioOpts{resync: time.Hour, tweak: permanentLeases})
	_, desc := s.mappedLB("lb")
	s.poll()
	s.f.ClearMappings() // PPPoE reconnect: mappings flushed, new WAN IP
	s.f.SetExternalIP("81.2.69.200")
	s.poll()
	if !s.rec.has("ExternalIPChanged") {
		t.Fatal("no ExternalIPChanged event")
	}
	if ip := getIGD(t).Status.ExternalIP; ip != "81.2.69.200" {
		t.Fatalf("status externalIP %q", ip)
	}
	s.eventually("immediate resync restored mappings", s.hasPorts(desc, "TCP/443->"+lbIP+":443", "TCP/32400->"+lbIP+":32400"))
}

func TestScenario_RouterRestartTriggersImmediateResync(t *testing.T) {
	s := startScenario(t, scenarioOpts{resync: time.Hour, tweak: permanentLeases})
	_, desc := s.mappedLB("lb")
	s.poll()
	s.f.RestartOnNewPort()
	s.eventually("restart detected", func() bool { s.poll(); return s.rec.has("RouterRestarted") })
	s.eventually("restored without waiting for the resync interval", s.hasPorts(desc, "TCP/443->"+lbIP+":443", "TCP/32400->"+lbIP+":32400"))
}

func TestScenario_LegacyCutover(t *testing.T) {
	s := startScenario(t, scenarioOpts{})
	s.f.AddMapping(fakeigd.Mapping{Protocol: "TCP", ExternalPort: 443, InternalPort: 443, InternalClient: lbIP, Enabled: true, Description: s.ns + "/public-traefik"})
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: s.ns, Name: "public-traefik", Finalizers: []string{LegacyFinalizer},
			Annotations: map[string]string{
				annotations.TCPEnabled: "true", annotations.TCPPorts: "443",
				"advertise.upnp/kopf-managed": "yes", "advertise.upnp/last-handled-configuration": "{}",
			},
		},
		Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer, Ports: []corev1.ServicePort{tcpPort(443)}},
	}
	if err := k8s.Create(bg, svc); err != nil {
		t.Fatal(err)
	}
	setIngress(t, svc, corev1.LoadBalancerIngress{IP: lbIP})
	s.eventually("legacy mapping adopted", s.hasPorts(mapping.DefaultDescriptions.For(mapping.Owner{Namespace: s.ns, Name: "public-traefik"}), "TCP/443->"+lbIP+":443"))
	got := getSvc(t, svc)
	if controllerutil.ContainsFinalizer(got, LegacyFinalizer) || got.Annotations["advertise.upnp/kopf-managed"] != "" {
		t.Fatalf("kopf metadata left: %v %v", got.Finalizers, got.Annotations)
	}
	if len(s.f.Mappings()) != 1 || s.rec.has("PortConflict") {
		t.Fatalf("mappings %v, conflict event %v", s.ports(""), s.rec.has("PortConflict"))
	}
}

func TestScenario_DoubleNAT(t *testing.T) {
	s := startScenario(t, scenarioOpts{})
	s.f.SetExternalIP("192.168.1.10")
	s.poll()
	c := meta.FindStatusCondition(getIGD(t).Status.Conditions, gatewayv1alpha1.ConditionPublicExternalIP)
	if c == nil || c.Status != metav1.ConditionFalse || !strings.Contains(c.Message, "192.168.1.10") {
		t.Fatalf("got %+v", c)
	}
}

func TestScenario_NoFlapOnReadFailure(t *testing.T) {
	s := startScenario(t, scenarioOpts{})
	s.mappedLB("lb")
	s.f.ResetRequests()
	s.f.SetHang(true)
	s.eventually("unreachable reconciles", func() bool {
		return testutil.ToFloat64(s.m.ReconcileTotal.WithLabelValues("router_unreachable")) >= 2
	})
	s.f.SetHang(false)
	s.eventually("recovered", func() bool { return testutil.ToFloat64(s.m.ReconcileTotal.WithLabelValues("success")) >= 3 })
	if n := s.f.Count("DeletePortMapping"); n != 0 {
		t.Fatalf("deletes %d after read failures", n)
	}
	if len(s.f.Mappings()) != 2 {
		t.Fatalf("mappings %v", s.ports(""))
	}
}

func TestScenario_LongServiceName_NoSelfConflict(t *testing.T) { // router keeps 63 bytes
	clk := clocktesting.NewFakeClock(time.Unix(1_000_000, 0))
	s := startScenario(t, scenarioOpts{fake: []fakeigd.Option{fakeigd.WithClock(clk)}, resync: time.Second})
	name := "a-service-name-that-is-long-enough-to-overflow-the-pf-label-xx" // 63 chars, the Service name maximum
	svc := createService(t, s.ns, name, corev1.ServiceTypeLoadBalancer,
		map[string]string{annotations.TCPEnabled: "true", annotations.TCPPorts: "443", annotations.LeaseSeconds: "10"}, tcpPort(443))
	setIngress(t, svc, corev1.LoadBalancerIngress{IP: lbIP})
	desc := mapping.DefaultDescriptions.For(mapping.Owner{Namespace: s.ns, Name: name})
	if len(mapping.DefaultPrefix)+len(s.ns)+1+len(name) <= mapping.MaxDescriptionLen {
		t.Fatalf("test name too short to exercise truncation")
	}
	s.eventually("mapped with a fitting description", s.hasPorts(desc, "TCP/443->"+lbIP+":443"))
	adds := s.f.Count("AddPortMapping")
	clk.Step(6 * time.Second) // past half of the 10s lease
	s.eventually("renewed", func() bool { return s.f.Count("AddPortMapping") > adds })
	if s.rec.has("PortConflict") {
		t.Fatalf("own mapping reported as a conflict: %v", s.rec.snapshot())
	}
}

func TestScenario_FormerPrefixAdopted(t *testing.T) { // cutover from upnp-nat-controller/ descriptions
	s := startScenario(t, scenarioOpts{})
	s.f.AddMapping(fakeigd.Mapping{Protocol: "TCP", ExternalPort: 443, InternalPort: 443, InternalClient: lbIP, Enabled: true,
		Description: mapping.FormerPrefix + s.ns + "/lb"})
	managedLB(t, s.ns, "lb", lbIP)
	desc := mapping.DefaultDescriptions.For(mapping.Owner{Namespace: s.ns, Name: "lb"})
	s.eventually("adopted under the new prefix", s.hasPorts(desc, "TCP/443->"+lbIP+":443", "TCP/32400->"+lbIP+":32400"))
	if len(s.f.Mappings()) != 2 || s.rec.has("PortConflict") {
		t.Fatalf("mappings %v, conflict %v", s.ports(""), s.rec.has("PortConflict"))
	}
}
