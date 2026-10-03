package metrics

import (
	"context"
	"errors"
	"sort"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"

	"github.com/nashant/upnp-nat-controller/internal/upnp"
	"github.com/nashant/upnp-nat-controller/internal/upnp/fake"
)

func TestMetrics_Registered(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	m := New(reg)
	// Touch every vector so it is gathered.
	m.ExternalIPInfo.WithLabelValues("1.2.3.4").Set(1)
	m.TrafficBytes.WithLabelValues("sent").Set(1)
	m.DesiredMappings.WithLabelValues("ns", "svc").Set(1)
	m.SOAPRequests.WithLabelValues("List").Inc()
	m.SOAPErrors.WithLabelValues("List", "unreachable").Inc()
	m.SOAPDuration.WithLabelValues("List").Observe(1)
	m.ReconcileTotal.WithLabelValues("success").Inc()

	fams, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range fams {
		got = append(got, f.GetName())
	}
	want := []string{
		"upnp_igd_discovered", "upnp_igd_uptime_seconds", "upnp_igd_external_ip_info", "upnp_igd_traffic_bytes_total",
		"upnp_port_mappings_total", "upnp_port_mappings_desired",
		"upnp_soap_requests_total", "upnp_soap_errors_total", "upnp_soap_request_duration_seconds",
		"upnp_reconcile_total", "upnp_router_restarts_total", "upnp_port_conflicts_total", "upnp_mapping_drift_repairs_total",
	}
	sort.Strings(got)
	sort.Strings(want)
	if d := cmp.Diff(want, got); d != "" {
		t.Fatalf("(-want +got)\n%s", d)
	}
}

func TestInstrument_CountsRequestsAndErrors(t *testing.T) {
	m := New(prometheus.NewRegistry())
	f := fake.New()
	c := Instrument(f, m)
	ctx := context.Background()
	_, _ = c.List(ctx)
	_, _ = c.Get(ctx, corev1.ProtocolTCP, 1) // ErrNoSuchEntry
	f.SetError("Add", errors.Join(upnp.ErrUnreachable))
	_ = c.Add(ctx, upnp.PortMapping{})
	_, _ = c.Status(ctx)
	_, _ = c.Traffic(ctx)
	_ = c.Delete(ctx, corev1.ProtocolTCP, 1)
	c.Invalidate()

	if v := testutil.ToFloat64(m.SOAPRequests.WithLabelValues("List")); v != 1 {
		t.Errorf("List requests %v", v)
	}
	if v := testutil.ToFloat64(m.SOAPErrors.WithLabelValues("Get", "no_such_entry")); v != 1 {
		t.Errorf("Get no_such_entry errors %v", v)
	}
	if v := testutil.ToFloat64(m.SOAPErrors.WithLabelValues("Add", "unreachable")); v != 1 {
		t.Errorf("Add unreachable errors %v", v)
	}
	if v := testutil.ToFloat64(m.SOAPErrors.WithLabelValues("Delete", "no_such_entry")); v != 1 {
		t.Errorf("Delete errors %v", v)
	}
	if f.Count("Invalidate") != 1 {
		t.Error("Invalidate not forwarded")
	}
	if n := testutil.CollectAndCount(m.SOAPDuration); n != 6 {
		t.Errorf("duration series %d, want 6", n)
	}
}

func TestErrorCode(t *testing.T) {
	for err, want := range map[error]string{
		upnp.ErrUnreachable: "unreachable", upnp.ErrNoSuchEntry: "no_such_entry", upnp.ErrConflict: "conflict",
		upnp.ErrEndOfList: "end_of_list", upnp.ErrNotAuthorized: "not_authorized", upnp.ErrActionFailed: "action_failed",
		upnp.ErrNoTrafficCounters: "unsupported", errors.New("x"): "other", context.DeadlineExceeded: "timeout",
	} {
		if got := errorCode(err); got != want {
			t.Errorf("errorCode(%v)=%q want %q", err, got, want)
		}
	}
}

func TestInstrument_StatusErrorCounted(t *testing.T) {
	m := New(prometheus.NewRegistry())
	f := fake.New()
	f.SetError("Status", upnp.ErrUnreachable)
	_, _ = Instrument(f, m).Status(context.Background())
	if v := testutil.ToFloat64(m.SOAPErrors.WithLabelValues("Status", "unreachable")); v != 1 {
		t.Fatalf("Status errors %v, want 1", v)
	}
}
