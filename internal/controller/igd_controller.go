// Package controller contains the IGD poller and the Service reconciler.
package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gatewayv1alpha1 "github.com/nashant/upnp-nat-controller/api/v1alpha1"
	"github.com/nashant/upnp-nat-controller/internal/ipclass"
	"github.com/nashant/upnp-nat-controller/internal/mapping"
	"github.com/nashant/upnp-nat-controller/internal/metrics"
	"github.com/nashant/upnp-nat-controller/internal/upnp"
)

// IGDName is the name of the singleton InternetGatewayDevice.
const IGDName = "default"

const (
	defaultPollingInterval = 30 * time.Second
	// lastSeenRefresh bounds how stale status.lastSeen may get (FR-IGD-4).
	lastSeenRefresh = 5 * time.Minute
)

// ResyncTrigger asks the Service reconciler to reconcile every managed Service now.
type ResyncTrigger interface {
	TriggerResync()
}

type observation struct {
	uptime     uint32
	location   string
	externalIP string
}

// IGDPoller polls the router, maintains the InternetGatewayDevice status,
// and triggers a resync when the router restarts or its WAN address changes.
// It runs as a manager.Runnable.
type IGDPoller struct {
	K8s      client.Client
	UPnP     upnp.Client
	Recorder events.EventRecorder
	Clock    clock.WithTicker
	Resync   ResyncTrigger
	Metrics  *metrics.Metrics
	Log      logr.Logger
	// OnPass, if set, is called after every poll with the delay until the next.
	OnPass func(next time.Duration)

	prev *observation
	// lostRouter is set when a poll fails, so the next good poll resyncs.
	lostRouter bool
}

// NeedLeaderElection implements manager.LeaderElectionRunnable.
func (p *IGDPoller) NeedLeaderElection() bool { return true }

// Start polls until ctx is done.
func (p *IGDPoller) Start(ctx context.Context) error {
	for {
		next, err := p.Poll(ctx)
		if err != nil {
			p.Log.Error(err, "IGD poll failed")
			next = defaultPollingInterval
		}
		if p.OnPass != nil {
			p.OnPass(next)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-p.Clock.After(next):
		}
	}
}

// Poll runs one poll and returns the delay until the next one. Router
// failures are recorded in the status, not returned; an error means the
// Kubernetes API failed.
func (p *IGDPoller) Poll(ctx context.Context) (time.Duration, error) {
	igd, err := p.ensureIGD(ctx)
	if err != nil {
		return 0, err
	}
	next := defaultPollingInterval
	if igd.Spec.PollingInterval > 0 {
		next = time.Duration(igd.Spec.PollingInterval) * time.Second
	}

	now := p.Clock.Now()
	status := *igd.Status.DeepCopy()
	st, err := p.UPnP.Status(ctx)
	if err != nil {
		p.recordUnreachable(&status, err, igd.Generation, now)
	} else {
		p.recordStatus(ctx, igd, &status, st, now)
	}

	if !statusChanged(igd.Status, status, now) {
		return next, nil
	}
	igd.Status = status
	if err := p.K8s.Status().Update(ctx, igd); err != nil {
		return 0, fmt.Errorf("update IGD status: %w", err)
	}
	return next, nil
}

func (p *IGDPoller) ensureIGD(ctx context.Context) (*gatewayv1alpha1.InternetGatewayDevice, error) {
	var igd gatewayv1alpha1.InternetGatewayDevice
	err := p.K8s.Get(ctx, client.ObjectKey{Name: IGDName}, &igd)
	if apierrors.IsNotFound(err) {
		igd = gatewayv1alpha1.InternetGatewayDevice{ObjectMeta: metav1.ObjectMeta{Name: IGDName}}
		if err := p.K8s.Create(ctx, &igd); err != nil {
			return nil, fmt.Errorf("create IGD: %w", err)
		}
		return &igd, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get IGD: %w", err)
	}
	return &igd, nil
}

func setCond(s *gatewayv1alpha1.InternetGatewayDeviceStatus, gen int64, now time.Time, typ string, ok bool, reason, msg string) {
	status := metav1.ConditionFalse
	if ok {
		status = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&s.Conditions, metav1.Condition{
		Type: typ, Status: status, Reason: reason, Message: msg,
		ObservedGeneration: gen, LastTransitionTime: metav1.NewTime(now),
	})
}

func (p *IGDPoller) recordUnreachable(s *gatewayv1alpha1.InternetGatewayDeviceStatus, err error, gen int64, now time.Time) {
	p.Log.V(1).Info("router unreachable", "error", err.Error())
	p.lostRouter = true
	p.Metrics.Discovered.Set(0)
	if errors.Is(err, upnp.ErrNoDevice) {
		setCond(s, gen, now, gatewayv1alpha1.ConditionDiscovered, false, "NotFound", "no Internet Gateway Device found")
	} else if meta.FindStatusCondition(s.Conditions, gatewayv1alpha1.ConditionDiscovered) == nil {
		setCond(s, gen, now, gatewayv1alpha1.ConditionDiscovered, false, "Unreachable", "router has not answered yet")
	}
	setCond(s, gen, now, gatewayv1alpha1.ConditionReachable, false, "Unreachable", "router did not answer the last poll")
	setCond(s, gen, now, gatewayv1alpha1.ConditionReady, false, "Unreachable", "router did not answer the last poll")
}

func (p *IGDPoller) recordStatus(ctx context.Context, igd *gatewayv1alpha1.InternetGatewayDevice, s *gatewayv1alpha1.InternetGatewayDeviceStatus, st upnp.DeviceStatus, now time.Time) {
	gen := igd.Generation
	if p.prev == nil && igd.Status.Location != "" {
		p.prev = &observation{uptime: uint32(igd.Status.Uptime), location: igd.Status.Location, externalIP: igd.Status.ExternalIP}
	}
	resync := p.lostRouter
	if prev := p.prev; prev != nil {
		if st.Uptime < prev.uptime || st.Location != prev.location {
			p.Recorder.Eventf(igd, nil, corev1.EventTypeWarning, "RouterRestarted", "Poll",
				"router restarted (uptime %ds -> %ds, location %s -> %s)", prev.uptime, st.Uptime, prev.location, st.Location)
			p.Metrics.RouterRestarts.Inc()
			p.Log.Info("router restarted", "location", st.Location, "uptime", st.Uptime)
			resync = true
		}
		if prev.externalIP != "" && st.ExternalIP != prev.externalIP {
			p.Recorder.Eventf(igd, nil, corev1.EventTypeNormal, "ExternalIPChanged", "Poll", "external IP changed from %s to %s", prev.externalIP, st.ExternalIP)
			p.Log.Info("external IP changed", "from", prev.externalIP, "to", st.ExternalIP)
			resync = true
		}
	}
	p.prev = &observation{uptime: st.Uptime, location: st.Location, externalIP: st.ExternalIP}
	p.lostRouter = false

	s.FriendlyName, s.Location, s.InternalIP = st.FriendlyName, st.Location, st.InternalIP
	s.ExternalIP, s.ConnectionStatus, s.Uptime = st.ExternalIP, st.ConnectionStatus, int64(st.Uptime)
	t := metav1.NewTime(now)
	s.LastSeen = &t

	connected := st.ConnectionStatus == "Connected"
	setCond(s, gen, now, gatewayv1alpha1.ConditionDiscovered, true, "Found", "found "+st.ServiceType)
	setCond(s, gen, now, gatewayv1alpha1.ConditionReachable, true, "Polled", "router answered the last poll")
	setCond(s, gen, now, gatewayv1alpha1.ConditionConnected, connected, "ConnectionStatus"+st.ConnectionStatus, "WAN connection status is "+st.ConnectionStatus)
	class := ipclass.ClassifyExternalIP(st.ExternalIP)
	msg := "external IP " + st.ExternalIP + " is public"
	if !class.IsPublic() {
		msg = fmt.Sprintf("external IP %q is %s: the router is probably behind another NAT and port mappings will not be reachable from the internet", st.ExternalIP, class)
	}
	setCond(s, gen, now, gatewayv1alpha1.ConditionPublicExternalIP, class.IsPublic(), string(class), msg)
	if connected {
		setCond(s, gen, now, gatewayv1alpha1.ConditionReady, true, "Ready", "router discovered, reachable and connected")
	} else {
		setCond(s, gen, now, gatewayv1alpha1.ConditionReady, false, "NotConnected", "WAN connection status is "+st.ConnectionStatus)
	}

	p.Metrics.Discovered.Set(1)
	p.Metrics.Uptime.Set(float64(st.Uptime))
	p.Metrics.ExternalIPInfo.Reset()
	p.Metrics.ExternalIPInfo.WithLabelValues(st.ExternalIP).Set(1)

	if tr, err := p.UPnP.Traffic(ctx); err == nil {
		p.Metrics.TrafficBytes.WithLabelValues("sent").Set(float64(tr.BytesSent))
		p.Metrics.TrafficBytes.WithLabelValues("received").Set(float64(tr.BytesReceived))
	}

	if list, err := p.UPnP.List(ctx); err == nil {
		s.PortMappings = ownedMappings(list)
		p.Metrics.OwnedMappings.Set(float64(len(s.PortMappings)))
	}

	if resync && p.Resync != nil {
		p.Resync.TriggerResync()
	}
}

func ownedMappings(list []upnp.PortMapping) []gatewayv1alpha1.PortMappingStatus {
	var out []gatewayv1alpha1.PortMappingStatus
	for _, m := range list {
		ns, name, ok := mapping.ParseOwnerDescription(m.Description)
		if !ok {
			continue
		}
		out = append(out, gatewayv1alpha1.PortMappingStatus{
			Protocol: string(m.Protocol), ExternalPort: int32(m.ExternalPort), InternalPort: int32(m.InternalPort),
			InternalClient: m.InternalClient, Enabled: m.Enabled, Description: m.Description,
			LeaseDuration: int64(m.LeaseDuration), ServiceRef: gatewayv1alpha1.ServiceRef{Namespace: ns, Name: name},
		})
	}
	return out
}

// statusChanged reports whether next must be written: a field other than
// the ones that tick every poll (lastSeen, uptime, remaining leases)
// changed, or lastSeen is older than lastSeenRefresh.
func statusChanged(old, next gatewayv1alpha1.InternetGatewayDeviceStatus, now time.Time) bool {
	if next.LastSeen != nil && (old.LastSeen == nil || now.Sub(old.LastSeen.Time) >= lastSeenRefresh) {
		return true
	}
	return !equality.Semantic.DeepEqual(stable(old), stable(next))
}

func stable(s gatewayv1alpha1.InternetGatewayDeviceStatus) gatewayv1alpha1.InternetGatewayDeviceStatus {
	s = *s.DeepCopy()
	s.LastSeen, s.Uptime = nil, 0
	for i := range s.PortMappings {
		s.PortMappings[i].LeaseDuration = 0
	}
	return s
}
