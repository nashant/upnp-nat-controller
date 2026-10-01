package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
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
