// Package metrics defines the controller's Prometheus metrics.
package metrics

import (
	"context"
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"

	"github.com/nashant/upnp-nat-controller/internal/upnp"
)

// Metrics holds every metric the controller exports.
type Metrics struct {
	Discovered      prometheus.Gauge
	Uptime          prometheus.Gauge
	ExternalIPInfo  *prometheus.GaugeVec
	TrafficBytes    *prometheus.GaugeVec
	OwnedMappings   prometheus.Gauge
	DesiredMappings *prometheus.GaugeVec
	SOAPRequests    *prometheus.CounterVec
	SOAPErrors      *prometheus.CounterVec
	SOAPDuration    *prometheus.HistogramVec
	ReconcileTotal  *prometheus.CounterVec
	RouterRestarts  prometheus.Counter
	PortConflicts   prometheus.Counter
	DriftRepairs    prometheus.Counter
}

// New creates the metrics and registers them with reg.
func New(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Discovered: prometheus.NewGauge(prometheus.GaugeOpts{Name: "upnp_igd_discovered", Help: "1 if the router was reachable on the last poll."}),
		Uptime:     prometheus.NewGauge(prometheus.GaugeOpts{Name: "upnp_igd_uptime_seconds", Help: "Router WAN connection uptime."}),
		ExternalIPInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "upnp_igd_external_ip_info", Help: "Router external IP (value is always 1)."},
			[]string{"ip"}),
		TrafficBytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "upnp_igd_traffic_bytes_total", Help: "Router WAN byte counters as reported by the router."},
			[]string{"direction"}),
		OwnedMappings: prometheus.NewGauge(prometheus.GaugeOpts{Name: "upnp_port_mappings_total", Help: "Port mappings on the router owned by this controller."}),
		DesiredMappings: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "upnp_port_mappings_desired", Help: "Port mappings requested by each Service."},
			[]string{"namespace", "service"}),
		SOAPRequests: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "upnp_soap_requests_total", Help: "Router client calls."},
			[]string{"method"}),
		SOAPErrors: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "upnp_soap_errors_total", Help: "Failed router client calls."},
			[]string{"method", "code"}),
		SOAPDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "upnp_soap_request_duration_seconds", Help: "Router client call latency.", Buckets: prometheus.DefBuckets},
			[]string{"method"}),
		ReconcileTotal: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "upnp_reconcile_total", Help: "Service reconciles by result."},
			[]string{"result"}),
		RouterRestarts: prometheus.NewCounter(prometheus.CounterOpts{Name: "upnp_router_restarts_total", Help: "Detected router restarts."}),
		PortConflicts:  prometheus.NewCounter(prometheus.CounterOpts{Name: "upnp_port_conflicts_total", Help: "Desired mappings blocked by a foreign mapping."}),
		DriftRepairs:   prometheus.NewCounter(prometheus.CounterOpts{Name: "upnp_mapping_drift_repairs_total", Help: "Mappings re-added because they were missing from the router."}),
	}
	reg.MustRegister(m.Discovered, m.Uptime, m.ExternalIPInfo, m.TrafficBytes, m.OwnedMappings, m.DesiredMappings,
		m.SOAPRequests, m.SOAPErrors, m.SOAPDuration, m.ReconcileTotal, m.RouterRestarts, m.PortConflicts, m.DriftRepairs)
	return m
}

func errorCode(err error) string {
	for code, target := range map[string]error{
		"unreachable": upnp.ErrUnreachable, "no_such_entry": upnp.ErrNoSuchEntry, "conflict": upnp.ErrConflict,
		"end_of_list": upnp.ErrEndOfList, "not_authorized": upnp.ErrNotAuthorized, "action_failed": upnp.ErrActionFailed,
		"unsupported": upnp.ErrNoTrafficCounters, "timeout": context.DeadlineExceeded,
	} {
		if errors.Is(err, target) {
			return code
		}
	}
	return "other"
}

type instrumented struct {
	c upnp.Client
	m *Metrics
}

// Instrument wraps c so every call is counted and timed.
func Instrument(c upnp.Client, m *Metrics) upnp.Client { return &instrumented{c: c, m: m} }

func (i *instrumented) observe(method string, start time.Time, err error) {
	i.m.SOAPRequests.WithLabelValues(method).Inc()
	i.m.SOAPDuration.WithLabelValues(method).Observe(time.Since(start).Seconds())
	if err != nil {
		i.m.SOAPErrors.WithLabelValues(method, errorCode(err)).Inc()
	}
}

func (i *instrumented) Status(ctx context.Context) (st upnp.DeviceStatus, err error) {
	defer func(t time.Time) { i.observe("Status", t, err) }(time.Now())
	return i.c.Status(ctx)
}

func (i *instrumented) Traffic(ctx context.Context) (tr upnp.TrafficStats, err error) {
	defer func(t time.Time) { i.observe("Traffic", t, err) }(time.Now())
	return i.c.Traffic(ctx)
}

func (i *instrumented) List(ctx context.Context) (ms []upnp.PortMapping, err error) {
	defer func(t time.Time) { i.observe("List", t, err) }(time.Now())
	return i.c.List(ctx)
}

func (i *instrumented) Get(ctx context.Context, p corev1.Protocol, ext uint16) (m upnp.PortMapping, err error) {
	defer func(t time.Time) { i.observe("Get", t, err) }(time.Now())
	return i.c.Get(ctx, p, ext)
}

func (i *instrumented) Add(ctx context.Context, pm upnp.PortMapping) (err error) {
	defer func(t time.Time) { i.observe("Add", t, err) }(time.Now())
	return i.c.Add(ctx, pm)
}

func (i *instrumented) Delete(ctx context.Context, p corev1.Protocol, ext uint16) (err error) {
	defer func(t time.Time) { i.observe("Delete", t, err) }(time.Now())
	return i.c.Delete(ctx, p, ext)
}

func (i *instrumented) Invalidate() { i.c.Invalidate() }
