package controller

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/nashant/upnp-nat-controller/internal/annotations"
)

// relevant reports whether the Service reconciler has anything to do for obj.
func relevant(obj client.Object) bool {
	if annotations.IsManaged(obj.GetAnnotations()) ||
		controllerutil.ContainsFinalizer(obj, Finalizer) || controllerutil.ContainsFinalizer(obj, LegacyFinalizer) {
		return true
	}
	for _, a := range legacyAnnotations {
		if _, ok := obj.GetAnnotations()[a]; ok {
			return true
		}
	}
	return false
}

// Resync is a ResyncTrigger whose Source enqueues every relevant Service.
type Resync struct {
	ch     chan event.GenericEvent
	reader client.Reader
}

var _ ResyncTrigger = (*Resync)(nil)

// NewResync returns a Resync that lists Services through reader.
func NewResync(reader client.Reader) *Resync {
	return &Resync{ch: make(chan event.GenericEvent, 1), reader: reader}
}

// TriggerResync implements ResyncTrigger. It never blocks; triggers that
// arrive while one is pending are coalesced.
func (r *Resync) TriggerResync() {
	select {
	case r.ch <- event.GenericEvent{Object: &corev1.Service{}}:
	default:
	}
}

// Source is the watch source to register with the Service controller.
func (r *Resync) Source() source.Source {
	return source.Channel(r.ch, handler.EnqueueRequestsFromMapFunc(r.requests))
}

func (r *Resync) requests(ctx context.Context, _ client.Object) []reconcile.Request {
	var list corev1.ServiceList
	if err := r.reader.List(ctx, &list); err != nil {
		log.FromContext(ctx).Error(err, "resync: list Services")
		return nil
	}
	var out []reconcile.Request
	for i := range list.Items {
		if relevant(&list.Items[i]) {
			out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
		}
	}
	return out
}

// HeartbeatKey is a request the Service reconciler answers without doing
// any work, so liveness can tell the workqueue is draining even when no
// Service is managed.
var HeartbeatKey = types.NamespacedName{Name: "upnp-nat-controller.heartbeat"}

// Heartbeat enqueues HeartbeatKey every interval. It is a leader-only
// manager.Runnable, like the Service controller it feeds.
type Heartbeat struct {
	clock    clock.WithTicker
	interval time.Duration
	ch       chan event.GenericEvent
}

// NewHeartbeat returns a heartbeat that fires every interval.
func NewHeartbeat(clk clock.WithTicker, interval time.Duration) *Heartbeat {
	return &Heartbeat{clock: clk, interval: interval, ch: make(chan event.GenericEvent, 1)}
}

// NeedLeaderElection implements manager.LeaderElectionRunnable.
func (h *Heartbeat) NeedLeaderElection() bool { return true }

// Start emits a heartbeat now and then every interval until ctx is done.
func (h *Heartbeat) Start(ctx context.Context) error {
	for {
		select {
		case h.ch <- event.GenericEvent{Object: &corev1.Service{}}:
		default:
		}
		select {
		case <-ctx.Done():
			return nil
		case <-h.clock.After(h.interval):
		}
	}
}

// Source is the watch source to register with the Service controller.
func (h *Heartbeat) Source() source.Source {
	return source.Channel(h.ch, handler.EnqueueRequestsFromMapFunc(h.requests))
}

func (h *Heartbeat) requests(context.Context, client.Object) []reconcile.Request {
	return []reconcile.Request{{NamespacedName: HeartbeatKey}}
}
