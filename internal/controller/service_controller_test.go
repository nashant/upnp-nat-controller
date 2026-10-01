package controller

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/nashant/upnp-nat-controller/internal/annotations"
	"github.com/nashant/upnp-nat-controller/internal/mapping"
	"github.com/nashant/upnp-nat-controller/internal/metrics"
	"github.com/nashant/upnp-nat-controller/internal/upnp"
	"github.com/nashant/upnp-nat-controller/internal/upnp/fake"
)

const lbIP = "172.16.1.2"

func newNamespace(t *testing.T) string {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "t-"}}
	if err := k8s.Create(bg, ns); err != nil {
		t.Fatal(err)
	}
	return ns.Name
}

func tcpPort(p int32) corev1.ServicePort {
	return corev1.ServicePort{Name: "p" + strconv.Itoa(int(p)), Protocol: corev1.ProtocolTCP, Port: p}
}

func udpPort(p int32) corev1.ServicePort {
	return corev1.ServicePort{Name: "u" + strconv.Itoa(int(p)), Protocol: corev1.ProtocolUDP, Port: p}
}

func createService(t *testing.T, ns, name string, typ corev1.ServiceType, ann map[string]string, ports ...corev1.ServicePort) *corev1.Service {
	t.Helper()
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Annotations: ann},
		Spec:       corev1.ServiceSpec{Type: typ, Ports: ports, Selector: map[string]string{"app": name}},
	}
	if err := k8s.Create(bg, svc); err != nil {
		t.Fatal(err)
	}
	return svc
}

func setIngress(t *testing.T, svc *corev1.Service, ingress ...corev1.LoadBalancerIngress) {
	t.Helper()
	if err := k8s.Get(bg, client.ObjectKeyFromObject(svc), svc); err != nil {
		t.Fatal(err)
	}
	svc.Status.LoadBalancer.Ingress = ingress
	if err := k8s.Status().Update(bg, svc); err != nil {
		t.Fatal(err)
	}
}

func managedLB(t *testing.T, ns, name, ip string) *corev1.Service {
	t.Helper()
	svc := createService(t, ns, name, corev1.ServiceTypeLoadBalancer,
		map[string]string{annotations.TCPEnabled: "true", annotations.TCPPorts: "443,32400"},
		tcpPort(443), tcpPort(32400))
	if ip != "" {
		setIngress(t, svc, corev1.LoadBalancerIngress{IP: ip})
	}
	return svc
}

type svcHarness struct {
	r      *ServiceReconciler
	router *fake.Client
	rec    *record.FakeRecorder
	clk    *clocktesting.FakeClock
	m      *metrics.Metrics
}

func newSvcHarness(t *testing.T) *svcHarness {
	t.Helper()
	h := &svcHarness{
		router: fake.New(),
		rec:    record.NewFakeRecorder(100),
		clk:    clocktesting.NewFakeClock(time.Now()),
		m:      metrics.New(prometheus.NewRegistry()),
	}
	h.r = &ServiceReconciler{
		K8s: k8s, UPnP: h.router, Recorder: h.rec, Clock: h.clk, Metrics: h.m,
		ResyncInterval: 30 * time.Second, RetryInterval: 10 * time.Second, IPWaitInterval: 5 * time.Second,
		DefaultLease: 3600, FinalizerTimeout: 10 * time.Minute,
	}
	return h
}

func (h *svcHarness) reconcile(t *testing.T, svc *corev1.Service) reconcile.Result {
	t.Helper()
	res, err := h.r.Reconcile(bg, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(svc)})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return res
}

func getSvc(t *testing.T, svc *corev1.Service) *corev1.Service {
	t.Helper()
	var got corev1.Service
	if err := k8s.Get(bg, client.ObjectKeyFromObject(svc), &got); err != nil {
		t.Fatal(err)
	}
	return &got
}

func routerCalls(c *fake.Client) int {
	n := 0
	for _, m := range []string{"Status", "Traffic", "List", "Get", "Add", "Delete"} {
		n += c.Count(m)
	}
	return n
}

func TestReconcile_UnmanagedService_NoRouterCalls(t *testing.T) { // FR-SVC-1
	h := newSvcHarness(t)
	ns := newNamespace(t)
	svc := createService(t, ns, "plain", corev1.ServiceTypeLoadBalancer, nil, tcpPort(80))
	setIngress(t, svc, corev1.LoadBalancerIngress{IP: lbIP})
	if res := h.reconcile(t, svc); res != (reconcile.Result{}) {
		t.Fatalf("result %+v, want no requeue", res)
	}
	if n := routerCalls(h.router); n != 0 {
		t.Fatalf("router calls %d", n)
	}
	if controllerutil.ContainsFinalizer(getSvc(t, svc), Finalizer) {
		t.Fatal("finalizer on unmanaged Service")
	}
}

func TestReconcile_MissingService_NoError(t *testing.T) {
	h := newSvcHarness(t)
	if _, err := h.r.Reconcile(bg, reconcile.Request{NamespacedName: client.ObjectKey{Namespace: "nope", Name: "gone"}}); err != nil {
		t.Fatal(err)
	}
}

func TestReconcile_NotLoadBalancer_WarningEvent(t *testing.T) { // FR-SVC-3, D13, S13
	h := newSvcHarness(t)
	ns := newNamespace(t)
	svc := createService(t, ns, "plex", corev1.ServiceTypeClusterIP,
		map[string]string{annotations.TCPEnabled: "true", annotations.TCPPorts: "32400"}, tcpPort(32400))
	if res := h.reconcile(t, svc); res != (reconcile.Result{}) {
		t.Fatalf("result %+v, want no requeue", res)
	}
	if !hasEvent(events(h.rec), "NotLoadBalancer") {
		t.Fatal("no NotLoadBalancer event")
	}
	if n := routerCalls(h.router); n != 0 {
		t.Fatalf("router calls %d", n)
	}
}

func TestReconcile_NoIngressIP_RequeuesAfter5s(t *testing.T) { // FR-SVC-4
	h := newSvcHarness(t)
	ns := newNamespace(t)
	svc := managedLB(t, ns, "lb", "")
	for i := 0; i < 3; i++ {
		if res := h.reconcile(t, svc); res.RequeueAfter != 5*time.Second {
			t.Fatalf("result %+v, want RequeueAfter 5s", res)
		}
	}
	evs := events(h.rec)
	n := 0
	for _, e := range evs {
		if hasEvent([]string{e}, "WaitingForIP") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("WaitingForIP events %d, want 1: %v", n, evs)
	}
	if routerCalls(h.router) != 0 {
		t.Fatal("router called without an IP")
	}
}

func TestReconcile_HostnameOnlyIngress_Warns(t *testing.T) { // FR-SVC-2
	h := newSvcHarness(t)
	ns := newNamespace(t)
	svc := managedLB(t, ns, "lb", "")
	setIngress(t, svc, corev1.LoadBalancerIngress{Hostname: "lb.example.com"}, corev1.LoadBalancerIngress{IP: "fd00::1"})
	if res := h.reconcile(t, svc); res.RequeueAfter == 0 {
		t.Fatalf("want requeue, got %+v", res)
	}
	if !hasEvent(events(h.rec), "NoIPv4Ingress") {
		t.Fatal("no NoIPv4Ingress event")
	}
}

func TestReconcile_FirstIPv4IngressUsed(t *testing.T) { // FR-SVC-2
	h := newSvcHarness(t)
	ns := newNamespace(t)
	svc := managedLB(t, ns, "lb", "")
	setIngress(t, svc, corev1.LoadBalancerIngress{IP: "fd00::1"}, corev1.LoadBalancerIngress{IP: lbIP}, corev1.LoadBalancerIngress{IP: "172.16.1.3"})
	h.reconcile(t, svc)
	ms := h.router.Mappings()
	if len(ms) != 2 || ms[0].InternalClient != lbIP {
		t.Fatalf("mappings %+v", ms)
	}
}

// finalizerCheckingRouter fails the test if Add runs before the Service has our finalizer.
type finalizerCheckingRouter struct {
	*fake.Client
	t   *testing.T
	svc client.ObjectKey
}

func (f *finalizerCheckingRouter) Add(ctx context.Context, m upnp.PortMapping) error {
	var svc corev1.Service
	if err := k8s.Get(ctx, f.svc, &svc); err != nil {
		f.t.Error(err)
	} else if !controllerutil.ContainsFinalizer(&svc, Finalizer) {
		f.t.Error("Add called before finalizer was persisted")
	}
	return f.Client.Add(ctx, m)
}

func TestReconcile_AddsFinalizerBeforeFirstMapping(t *testing.T) { // FR-SVC-9
	h := newSvcHarness(t)
	ns := newNamespace(t)
	svc := managedLB(t, ns, "lb", lbIP)
	h.r.UPnP = &finalizerCheckingRouter{Client: h.router, t: t, svc: client.ObjectKeyFromObject(svc)}
	h.reconcile(t, svc)
	if h.router.Count("Add") != 2 {
		t.Fatalf("adds %d", h.router.Count("Add"))
	}
	if !controllerutil.ContainsFinalizer(getSvc(t, svc), Finalizer) {
		t.Fatal("finalizer missing")
	}
}

func TestReconcile_Success_RequeueAfterResyncInterval(t *testing.T) { // FR-SVC-8, D4
	h := newSvcHarness(t)
	ns := newNamespace(t)
	svc := managedLB(t, ns, "public-traefik", lbIP)
	if res := h.reconcile(t, svc); res.RequeueAfter != 30*time.Second {
		t.Fatalf("result %+v, want RequeueAfter 30s", res)
	}
	want := []upnp.PortMapping{
		{Protocol: corev1.ProtocolTCP, ExternalPort: 443, InternalPort: 443, InternalClient: lbIP, Enabled: true, Description: mapping.OwnerDescription(ns, "public-traefik"), LeaseDuration: 3600},
		{Protocol: corev1.ProtocolTCP, ExternalPort: 32400, InternalPort: 32400, InternalClient: lbIP, Enabled: true, Description: mapping.OwnerDescription(ns, "public-traefik"), LeaseDuration: 3600},
	}
	if got := h.router.Mappings(); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("mappings %+v", got)
	}
	// Second pass: nothing to do, still requeued.
	h.router.ResetCalls()
	if res := h.reconcile(t, svc); res.RequeueAfter != 30*time.Second {
		t.Fatalf("result %+v", res)
	}
	if h.router.Count("Add")+h.router.Count("Delete") != 0 {
		t.Fatal("no-op pass changed the router")
	}
	if v := testutil.ToFloat64(h.m.ReconcileTotal.WithLabelValues("success")); v != 2 {
		t.Fatalf("reconcile_total{success} %v", v)
	}
	if v := testutil.ToFloat64(h.m.DesiredMappings.WithLabelValues(ns, "public-traefik")); v != 2 {
		t.Fatalf("desired %v", v)
	}
}

func TestReconcile_LeaseAnnotation(t *testing.T) { // FR-ANN-5
	h := newSvcHarness(t)
	ns := newNamespace(t)
	svc := createService(t, ns, "lb", corev1.ServiceTypeLoadBalancer,
		map[string]string{annotations.TCPEnabled: "true", annotations.TCPPorts: "443", annotations.LeaseSeconds: "0"}, tcpPort(443))
	setIngress(t, svc, corev1.LoadBalancerIngress{IP: lbIP})
	h.reconcile(t, svc)
	if ms := h.router.Mappings(); len(ms) != 1 || ms[0].LeaseDuration != 0 {
		t.Fatalf("got %+v", ms)
	}
}

func TestReconcile_RouterRebooted_ReAddsAndCountsDrift(t *testing.T) { // FR-SVC-8, S1, NFR-OPS-3
	h := newSvcHarness(t)
	ns := newNamespace(t)
	svc := managedLB(t, ns, "lb", lbIP)
	h.reconcile(t, svc)
	h.router.Clear()
	h.reconcile(t, svc)
	if len(h.router.Mappings()) != 2 {
		t.Fatal("mappings not restored")
	}
	if v := testutil.ToFloat64(h.m.DriftRepairs); v != 2 {
		t.Fatalf("drift repairs %v, want 2", v)
	}
}

func TestReconcile_ReadFailure_NoDeletes_ShortRequeue(t *testing.T) { // FR-SVC-12, S18
	h := newSvcHarness(t)
	ns := newNamespace(t)
	svc := managedLB(t, ns, "lb", lbIP)
	h.reconcile(t, svc)
	h.router.SetError("List", upnp.ErrUnreachable)
	h.router.ResetCalls()
	res, err := h.r.Reconcile(bg, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(svc)})
	if err != nil || res.RequeueAfter != 10*time.Second {
		t.Fatalf("got %+v, %v; want RequeueAfter 10s, nil", res, err)
	}
	if h.router.Count("Delete")+h.router.Count("Add") != 0 {
		t.Fatal("router modified after a failed read")
	}
	if v := testutil.ToFloat64(h.m.ReconcileTotal.WithLabelValues("router_unreachable")); v != 1 {
		t.Fatalf("reconcile_total{router_unreachable} %v", v)
	}
}

func TestReconcile_WriteFailureUnreachable_ShortRequeue(t *testing.T) { // FR-SVC-12
	h := newSvcHarness(t)
	ns := newNamespace(t)
	svc := managedLB(t, ns, "lb", lbIP)
	h.router.SetError("Add", upnp.ErrUnreachable)
	if res := h.reconcile(t, svc); res.RequeueAfter != 10*time.Second {
		t.Fatalf("got %+v", res)
	}
	if h.router.Count("Add") != 1 {
		t.Fatalf("kept going after unreachable: %d adds", h.router.Count("Add"))
	}
}

func TestReconcile_Conflict_EventAndOtherPortsMapped(t *testing.T) { // FR-SVC-6, S9
	h := newSvcHarness(t)
	ns := newNamespace(t)
	foreign := upnp.PortMapping{Protocol: corev1.ProtocolTCP, ExternalPort: 443, InternalPort: 443, InternalClient: "192.168.1.50", Enabled: true, Description: "Xbox"}
	h.router.Seed(foreign)
	svc := managedLB(t, ns, "lb", lbIP)
	if res := h.reconcile(t, svc); res.RequeueAfter != 30*time.Second {
		t.Fatalf("got %+v", res)
	}
	if !hasEvent(events(h.rec), "PortConflict") {
		t.Fatal("no PortConflict event")
	}
	ms := h.router.Mappings()
	if len(ms) != 2 || ms[0] != foreign || ms[1].ExternalPort != 32400 {
		t.Fatalf("mappings %+v", ms)
	}
	if v := testutil.ToFloat64(h.m.PortConflicts); v != 1 {
		t.Fatalf("conflicts %v", v)
	}
}

func TestReconcile_AddRaceConflict_Event(t *testing.T) { // FR-SVC-6: 718 from Add
	h := newSvcHarness(t)
	ns := newNamespace(t)
	svc := managedLB(t, ns, "lb", lbIP)
	h.router.SetError("Add", upnp.ErrConflict)
	h.reconcile(t, svc)
	if !hasEvent(events(h.rec), "PortConflict") {
		t.Fatal("no PortConflict event")
	}
}

func TestReconcile_OtherRouterError_EventAndContinue(t *testing.T) {
	h := newSvcHarness(t)
	ns := newNamespace(t)
	svc := managedLB(t, ns, "lb", lbIP)
	h.router.SetError("Add", upnp.ErrNotAuthorized)
	if res := h.reconcile(t, svc); res.RequeueAfter != 30*time.Second {
		t.Fatalf("got %+v", res)
	}
	if h.router.Count("Add") != 2 {
		t.Fatalf("adds %d, want both attempted", h.router.Count("Add"))
	}
	if !hasEvent(events(h.rec), "MappingFailed") {
		t.Fatal("no MappingFailed event")
	}
}

func TestReconcile_InvalidAnnotations_WarningEvent(t *testing.T) { // FR-ANN-*
	h := newSvcHarness(t)
	ns := newNamespace(t)
	svc := createService(t, ns, "lb", corev1.ServiceTypeLoadBalancer,
		map[string]string{annotations.TCPEnabled: "true", annotations.TCPPorts: "443,abc"}, tcpPort(443))
	setIngress(t, svc, corev1.LoadBalancerIngress{IP: lbIP})
	if res := h.reconcile(t, svc); res != (reconcile.Result{}) {
		t.Fatalf("got %+v, want no requeue", res)
	}
	if !hasEvent(events(h.rec), "InvalidAnnotations") {
		t.Fatal("no InvalidAnnotations event")
	}
	if routerCalls(h.router) != 0 {
		t.Fatal("router called with invalid annotations")
	}
}

func TestReconcile_LBIPChange_Replaces(t *testing.T) { // FR-SVC-6, S10
	h := newSvcHarness(t)
	ns := newNamespace(t)
	svc := managedLB(t, ns, "lb", lbIP)
	h.reconcile(t, svc)
	setIngress(t, svc, corev1.LoadBalancerIngress{IP: "172.16.1.9"})
	h.reconcile(t, svc)
	for _, m := range h.router.Mappings() {
		if m.InternalClient != "172.16.1.9" {
			t.Fatalf("mapping %+v not moved", m)
		}
	}
	if h.router.Count("Delete") != 2 {
		t.Fatalf("deletes %d, want 2 (replace)", h.router.Count("Delete"))
	}
}

func TestReconcile_PortListChanged_DeletesOnlyRemoved(t *testing.T) { // FR-SVC-7, S12
	h := newSvcHarness(t)
	ns := newNamespace(t)
	svc := managedLB(t, ns, "lb", lbIP)
	h.reconcile(t, svc)
	svc = getSvc(t, svc)
	svc.Annotations[annotations.TCPPorts] = "443"
	if err := k8s.Update(bg, svc); err != nil {
		t.Fatal(err)
	}
	h.router.ResetCalls()
	h.reconcile(t, svc)
	if h.router.Count("Add") != 0 || h.router.Count("Delete") != 1 {
		t.Fatalf("adds %d deletes %d, want 0/1", h.router.Count("Add"), h.router.Count("Delete"))
	}
	if ms := h.router.Mappings(); len(ms) != 1 || ms[0].ExternalPort != 443 {
		t.Fatalf("mappings %+v", ms)
	}
}

func TestReconcile_AnnotationRemoved_CleansUp(t *testing.T) { // FR-SVC-9, S11
	h := newSvcHarness(t)
	ns := newNamespace(t)
	svc := managedLB(t, ns, "lb", lbIP)
	h.reconcile(t, svc)
	svc = getSvc(t, svc)
	delete(svc.Annotations, annotations.TCPEnabled)
	if err := k8s.Update(bg, svc); err != nil {
		t.Fatal(err)
	}
	if res := h.reconcile(t, svc); res != (reconcile.Result{}) {
		t.Fatalf("got %+v", res)
	}
	if len(h.router.Mappings()) != 0 {
		t.Fatalf("mappings left %+v", h.router.Mappings())
	}
	if controllerutil.ContainsFinalizer(getSvc(t, svc), Finalizer) {
		t.Fatal("finalizer not removed")
	}
}

func TestReconcile_TypeChangedToClusterIP_CleansUp(t *testing.T) { // FR-SVC-9
	h := newSvcHarness(t)
	ns := newNamespace(t)
	svc := managedLB(t, ns, "lb", lbIP)
	h.reconcile(t, svc)
	svc = getSvc(t, svc)
	svc.Spec.Type = corev1.ServiceTypeClusterIP
	for i := range svc.Spec.Ports {
		svc.Spec.Ports[i].NodePort = 0
	}
	if err := k8s.Update(bg, svc); err != nil {
		t.Fatal(err)
	}
	h.reconcile(t, svc)
	if len(h.router.Mappings()) != 0 || controllerutil.ContainsFinalizer(getSvc(t, svc), Finalizer) {
		t.Fatal("not cleaned up")
	}
	if !hasEvent(events(h.rec), "NotLoadBalancer") {
		t.Fatal("no NotLoadBalancer event")
	}
}

func TestReconcile_Delete_RemovesMappingsThenFinalizer(t *testing.T) { // FR-SVC-9, D7, S7
	h := newSvcHarness(t)
	ns := newNamespace(t)
	other := upnp.PortMapping{Protocol: corev1.ProtocolTCP, ExternalPort: 80, InternalPort: 80, InternalClient: "172.16.1.3", Enabled: true, Description: mapping.OwnerDescription(ns, "other")}
	h.router.Seed(other)
	svc := managedLB(t, ns, "lb", lbIP)
	h.reconcile(t, svc)
	if err := k8s.Delete(bg, svc); err != nil {
		t.Fatal(err)
	}
	h.reconcile(t, svc)
	if ms := h.router.Mappings(); len(ms) != 1 || ms[0] != other {
		t.Fatalf("mappings %+v, want only the other Service's", ms)
	}
	if err := k8s.Get(bg, client.ObjectKeyFromObject(svc), &corev1.Service{}); !apierrors.IsNotFound(err) {
		t.Fatalf("Service not gone: %v", err)
	}
	if n := testutil.CollectAndCount(h.m.DesiredMappings); n != 0 {
		t.Fatalf("desired series left: %d", n)
	}
}

func TestReconcile_Delete_RouterDown_TimeoutReleasesFinalizer(t *testing.T) { // FR-SVC-9, S8
	h := newSvcHarness(t)
	ns := newNamespace(t)
	svc := managedLB(t, ns, "lb", lbIP)
	h.reconcile(t, svc)
	if err := k8s.Delete(bg, svc); err != nil {
		t.Fatal(err)
	}
	h.router.SetError("List", upnp.ErrUnreachable)
	if res := h.reconcile(t, svc); res.RequeueAfter != 10*time.Second {
		t.Fatalf("got %+v, want retry", res)
	}
	if !controllerutil.ContainsFinalizer(getSvc(t, svc), Finalizer) {
		t.Fatal("finalizer released before timeout")
	}
	h.clk.Step(11 * time.Minute)
	h.reconcile(t, svc)
	if err := k8s.Get(bg, client.ObjectKeyFromObject(svc), &corev1.Service{}); !apierrors.IsNotFound(err) {
		t.Fatalf("Service not gone after timeout: %v", err)
	}
	if !hasEvent(events(h.rec), "OrphanedMappings") {
		t.Fatal("no OrphanedMappings event")
	}
}

func TestReconcile_RemovesLegacyKopfFinalizerAndAnnotations(t *testing.T) { // FR-SVC-10, S15
	h := newSvcHarness(t)
	ns := newNamespace(t)
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: "public-traefik",
			Finalizers: []string{LegacyFinalizer, "example.com/keep"},
			Annotations: map[string]string{
				annotations.TCPEnabled: "true", annotations.TCPPorts: "443",
				"advertise.upnp/kopf-managed": "yes", "advertise.upnp/last-handled-configuration": "{}",
				"unrelated": "x",
			},
		},
		Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer, Ports: []corev1.ServicePort{tcpPort(443)}},
	}
	if err := k8s.Create(bg, svc); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { // release the foreign finalizer so the namespace can go
		s := getSvc(t, svc)
		controllerutil.RemoveFinalizer(s, "example.com/keep")
		_ = k8s.Update(bg, s)
	})
	setIngress(t, svc, corev1.LoadBalancerIngress{IP: lbIP})
	legacy := upnp.PortMapping{Protocol: corev1.ProtocolTCP, ExternalPort: 443, InternalPort: 443, InternalClient: lbIP, Enabled: true, Description: ns + "/public-traefik", LeaseDuration: 604000}
	h.router.Seed(legacy)

	h.reconcile(t, svc)
	h.reconcile(t, svc) // idempotent
	got := getSvc(t, svc)
	if controllerutil.ContainsFinalizer(got, LegacyFinalizer) || !controllerutil.ContainsFinalizer(got, "example.com/keep") || !controllerutil.ContainsFinalizer(got, Finalizer) {
		t.Fatalf("finalizers %v", got.Finalizers)
	}
	for _, a := range []string{"advertise.upnp/kopf-managed", "advertise.upnp/last-handled-configuration"} {
		if _, ok := got.Annotations[a]; ok {
			t.Fatalf("annotation %s not removed", a)
		}
	}
	if got.Annotations["unrelated"] != "x" {
		t.Fatal("unrelated annotation removed")
	}
	if ms := h.router.Mappings(); len(ms) != 1 || ms[0].Description != mapping.OwnerDescription(ns, "public-traefik") {
		t.Fatalf("legacy mapping not adopted: %+v", ms)
	}
	if hasEvent(events(h.rec), "PortConflict") {
		t.Fatal("PortConflict against our own legacy mapping")
	}
}

func TestReconcile_LegacyCleanupOnUnmanagedService(t *testing.T) { // FR-SVC-10: any Service carrying them
	h := newSvcHarness(t)
	ns := newNamespace(t)
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "old", Finalizers: []string{LegacyFinalizer}},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, Ports: []corev1.ServicePort{tcpPort(80)}},
	}
	if err := k8s.Create(bg, svc); err != nil {
		t.Fatal(err)
	}
	h.reconcile(t, svc)
	if controllerutil.ContainsFinalizer(getSvc(t, svc), LegacyFinalizer) {
		t.Fatal("kopf finalizer not removed")
	}
}

func TestReconcile_ReadFailureDuringCleanup_Retries(t *testing.T) { // FR-SVC-12
	h := newSvcHarness(t)
	ns := newNamespace(t)
	svc := managedLB(t, ns, "lb", lbIP)
	h.reconcile(t, svc)
	svc = getSvc(t, svc)
	delete(svc.Annotations, annotations.TCPEnabled)
	if err := k8s.Update(bg, svc); err != nil {
		t.Fatal(err)
	}
	h.router.SetError("List", errors.Join(upnp.ErrUnreachable))
	if res := h.reconcile(t, svc); res.RequeueAfter != 10*time.Second {
		t.Fatalf("got %+v", res)
	}
	if !controllerutil.ContainsFinalizer(getSvc(t, svc), Finalizer) {
		t.Fatal("finalizer removed although mappings were not deleted")
	}
}

func TestReconcile_UDPOnly(t *testing.T) { // D8, S6
	h := newSvcHarness(t)
	ns := newNamespace(t)
	svc := createService(t, ns, "wg", corev1.ServiceTypeLoadBalancer,
		map[string]string{annotations.UDPEnabled: "true", annotations.UDPPorts: "51820"}, udpPort(51820))
	setIngress(t, svc, corev1.LoadBalancerIngress{IP: lbIP})
	h.reconcile(t, svc)
	if ms := h.router.Mappings(); len(ms) != 1 || ms[0].Protocol != corev1.ProtocolUDP {
		t.Fatalf("got %+v", ms)
	}
}
