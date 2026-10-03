package controller

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/nashant/upnp-nat-controller/internal/annotations"
)

func TestResync_EnqueuesAllManaged(t *testing.T) {
	ns := newNamespace(t)
	managedLB(t, ns, "a", "")
	createService(t, ns, "udp", corev1.ServiceTypeLoadBalancer, map[string]string{annotations.UDPEnabled: "true"}, udpPort(53))
	createService(t, ns, "plain", corev1.ServiceTypeLoadBalancer, nil, tcpPort(80))
	withFinalizer := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "unannotated-with-finalizer", Finalizers: []string{Finalizer}},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{tcpPort(80)}},
	}
	if err := k8s.Create(bg, withFinalizer); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		s := getSvc(t, withFinalizer)
		s.Finalizers = nil
		_ = k8s.Update(bg, s)
	})

	var got []string
	for _, r := range NewResync(k8s).requests(bg, nil) {
		if r.Namespace == ns {
			got = append(got, r.Name)
		}
	}
	sort.Strings(got)
	if d := cmp.Diff([]string{"a", "udp", "unannotated-with-finalizer"}, got); d != "" {
		t.Fatalf("(-want +got)\n%s", d)
	}
}

func TestResync_TriggerNeverBlocks(t *testing.T) {
	r := NewResync(k8s)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			r.TriggerResync()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("TriggerResync blocked with nobody reading")
	}
}

func TestRelevant(t *testing.T) {
	cases := map[string]struct {
		meta metav1.ObjectMeta
		want bool
	}{
		"none":       {metav1.ObjectMeta{}, false},
		"managed":    {metav1.ObjectMeta{Annotations: map[string]string{annotations.TCPEnabled: "true"}}, true},
		"finalizer":  {metav1.ObjectMeta{Finalizers: []string{Finalizer}}, true},
		"kopf":       {metav1.ObjectMeta{Finalizers: []string{LegacyFinalizer}}, true},
		"kopf annot": {metav1.ObjectMeta{Annotations: map[string]string{"advertise.upnp/kopf-managed": "yes"}}, true},
		"disabled":   {metav1.ObjectMeta{Annotations: map[string]string{annotations.TCPEnabled: "false"}}, false},
	}
	for name, c := range cases {
		if got := relevant(&corev1.Service{ObjectMeta: c.meta}); got != c.want {
			t.Errorf("%s: relevant=%v want %v", name, got, c.want)
		}
	}
}

func TestReconcile_HeartbeatCallsOnPassOnly(t *testing.T) {
	h := newSvcHarness(t)
	passes := 0
	h.r.OnPass = func() { passes++ }
	res, err := h.r.Reconcile(bg, reconcile.Request{NamespacedName: HeartbeatKey})
	if err != nil || res != (reconcile.Result{}) || passes != 1 {
		t.Fatalf("res %+v err %v passes %d", res, err, passes)
	}
	if routerCalls(h.router) != 0 {
		t.Fatal("heartbeat touched the router")
	}
}

func TestHeartbeat_EmitsEveryInterval(t *testing.T) {
	clk := clocktesting.NewFakeClock(time.Unix(1_000_000, 0))
	hb := NewHeartbeat(clk, 30*time.Second)
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	go func() { _ = hb.Start(ctx) }()
	recv := func() bool {
		select {
		case <-hb.ch:
			return true
		case <-time.After(2 * time.Second):
			return false
		}
	}
	if !recv() {
		t.Fatal("no heartbeat at start")
	}
	for !clk.HasWaiters() {
		time.Sleep(time.Millisecond)
	}
	clk.Step(30 * time.Second)
	if !recv() {
		t.Fatal("no heartbeat after interval")
	}
	if !hb.NeedLeaderElection() {
		t.Fatal("heartbeat must follow the leader-only controller")
	}
	if got := hb.requests(ctx, nil); len(got) != 1 || got[0].NamespacedName != HeartbeatKey {
		t.Fatalf("requests %v", got)
	}
}
