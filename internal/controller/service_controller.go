package controller

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/nashant/upnp-nat-controller/internal/annotations"
	"github.com/nashant/upnp-nat-controller/internal/mapping"
	"github.com/nashant/upnp-nat-controller/internal/metrics"
	"github.com/nashant/upnp-nat-controller/internal/upnp"
)

// Finalizers and legacy (Python/kopf controller) metadata.
const (
	Finalizer       = "upnp.nashes.uk/port-mappings"
	LegacyFinalizer = "advertise.upnp/kopf-finalizer"
)

var legacyAnnotations = []string{"advertise.upnp/kopf-managed", "advertise.upnp/last-handled-configuration"}

// Reconcile results, as the upnp_reconcile_total "result" label.
const (
	resultSuccess     = "success"
	resultUnreachable = "router_unreachable"
	resultWaiting     = "waiting"
	resultInvalid     = "invalid"
	resultSkipped     = "skipped"
	resultCleanup     = "cleanup"
	resultError       = "error"
)

// ServiceReconciler keeps the router's port mappings in line with the
// annotations of each managed LoadBalancer Service. Every successful pass
// requeues after ResyncInterval, so mappings lost on the router side are
// restored without any Service event.
type ServiceReconciler struct {
	K8s      client.Client
	UPnP     upnp.Client
	Recorder record.EventRecorder
	Clock    clock.PassiveClock
	Metrics  *metrics.Metrics
	// OnPass, if set, is called after every reconcile that ran to completion.
	OnPass func()

	ResyncInterval   time.Duration // requeue after a good pass (30s)
	RetryInterval    time.Duration // requeue while the router is unreachable (10s)
	IPWaitInterval   time.Duration // requeue while waiting for an LB IP (5s)
	DefaultLease     uint32        // seconds (3600)
	FinalizerTimeout time.Duration // give up on cleanup during deletion (10m)

	mu sync.Mutex
	// waiting holds Services that already got a WaitingForIP event.
	waiting map[types.UID]bool
	// seen holds the mappings each Service had on the router at its last
	// pass, to tell a drift repair from a first add.
	seen map[types.UID]map[string]bool
}

func mappingKey(proto corev1.Protocol, port uint16) string { return fmt.Sprintf("%s/%d", proto, port) }

// Reconcile implements reconcile.Reconciler.
func (r *ServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	res, result, err := r.reconcile(ctx, req)
	if err != nil {
		result = resultError
	}
	if result != "" {
		r.Metrics.ReconcileTotal.WithLabelValues(result).Inc()
	}
	if err == nil && r.OnPass != nil {
		r.OnPass()
	}
	return res, err
}

func (r *ServiceReconciler) reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, string, error) {
	logger := log.FromContext(ctx)
	var svc corev1.Service
	if err := r.K8s.Get(ctx, req.NamespacedName, &svc); err != nil {
		if apierrors.IsNotFound(err) {
			r.forget(req.NamespacedName, "")
			return ctrl.Result{}, "", nil
		}
		return ctrl.Result{}, "", err
	}

	if err := r.removeLegacyMetadata(ctx, &svc); err != nil {
		return ctrl.Result{}, "", err
	}

	managed := annotations.IsManaged(svc.Annotations)
	isLB := svc.Spec.Type == corev1.ServiceTypeLoadBalancer
	if managed && !isLB {
		r.Recorder.Eventf(&svc, corev1.EventTypeWarning, "NotLoadBalancer",
			"Service has %s annotations but type %s; only LoadBalancer Services are mapped", "advertise.upnp", svc.Spec.Type)
	}
	if !managed || !isLB || !svc.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(&svc, Finalizer) {
			return ctrl.Result{}, resultSkippedIf(managed), nil
		}
		return r.cleanup(ctx, logger, &svc)
	}

	spec, err := annotations.Parse(svc.Annotations, svc.Spec.Ports, r.DefaultLease)
	if err != nil {
		r.Recorder.Eventf(&svc, corev1.EventTypeWarning, "InvalidAnnotations", "%v", err)
		return ctrl.Result{}, resultInvalid, nil
	}

	ip, ok := r.ingressIP(&svc)
	if !ok {
		return ctrl.Result{RequeueAfter: r.IPWaitInterval}, resultWaiting, nil
	}

	if controllerutil.AddFinalizer(&svc, Finalizer) {
		if err := r.K8s.Update(ctx, &svc); err != nil {
			return ctrl.Result{}, "", fmt.Errorf("add finalizer: %w", err)
		}
	}

	desired := make([]mapping.Desired, 0, len(spec.Mappings))
	for _, d := range spec.Mappings {
		desired = append(desired, mapping.Desired{
			Protocol: d.Protocol, ExternalPort: d.ExternalPort, InternalPort: d.InternalPort,
			InternalClient: ip, LeaseSeconds: spec.LeaseSeconds,
		})
	}
	r.Metrics.DesiredMappings.WithLabelValues(svc.Namespace, svc.Name).Set(float64(len(desired)))

	actual, err := r.UPnP.List(ctx)
	if err != nil {
		logger.V(1).Info("router unreachable, will retry", "error", err.Error())
		return ctrl.Result{RequeueAfter: r.RetryInterval}, resultUnreachable, nil
	}
	if r.apply(ctx, logger, &svc, desired, actual) {
		return ctrl.Result{RequeueAfter: r.RetryInterval}, resultUnreachable, nil
	}
	return ctrl.Result{RequeueAfter: r.ResyncInterval}, resultSuccess, nil
}

func resultSkippedIf(managed bool) string {
	if managed {
		return resultSkipped
	}
	return ""
}

// ingressIP returns the first IPv4 ingress address. It emits one
// WaitingForIP event per Service while there is no ingress at all.
func (r *ServiceReconciler) ingressIP(svc *corev1.Service) (string, bool) {
	ingress := svc.Status.LoadBalancer.Ingress
	for _, in := range ingress {
		if a, err := netip.ParseAddr(in.IP); err == nil && a.Is4() {
			r.mu.Lock()
			delete(r.waiting, svc.UID)
			r.mu.Unlock()
			return a.String(), true
		}
	}
	if len(ingress) > 0 {
		r.Recorder.Event(svc, corev1.EventTypeWarning, "NoIPv4Ingress", "LoadBalancer ingress has no IPv4 address; UPnP mappings need one")
		return "", false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.waiting == nil {
		r.waiting = map[types.UID]bool{}
	}
	if !r.waiting[svc.UID] {
		r.waiting[svc.UID] = true
		r.Recorder.Event(svc, corev1.EventTypeNormal, "WaitingForIP", "waiting for a LoadBalancer IP")
	}
	return "", false
}

// apply executes the plan for svc. It reports whether the router became
// unreachable part-way, in which case the remaining actions are skipped.
func (r *ServiceReconciler) apply(ctx context.Context, logger logr.Logger, svc *corev1.Service, desired []mapping.Desired, actual []upnp.PortMapping) (unreachable bool) {
	owner := mapping.Owner{Namespace: svc.Namespace, Name: svc.Name}
	r.mu.Lock()
	prevSeen := r.seen[svc.UID]
	r.mu.Unlock()
	nowSeen := map[string]bool{}
	for _, d := range desired {
		for _, a := range actual {
			if a.Protocol == d.Protocol && a.ExternalPort == d.ExternalPort {
				nowSeen[mappingKey(d.Protocol, d.ExternalPort)] = true
			}
		}
	}

	for _, act := range mapping.Plan(owner, desired, actual, 2*r.ResyncInterval) {
		var err error
		m := act.Mapping
		switch act.Kind {
		case mapping.Conflict:
			r.conflict(svc, m, act.Existing)
			continue
		case mapping.Add, mapping.Renew:
			err = r.UPnP.Add(ctx, m)
			if err == nil && act.Kind == mapping.Add && prevSeen[mappingKey(m.Protocol, m.ExternalPort)] {
				r.Metrics.DriftRepairs.Inc()
				logger.Info("re-added missing port mapping", "protocol", m.Protocol, "externalPort", m.ExternalPort)
			} else if err == nil && act.Kind == mapping.Add {
				logger.Info("added port mapping", "protocol", m.Protocol, "externalPort", m.ExternalPort, "internalClient", m.InternalClient, "internalPort", m.InternalPort)
			} else if err == nil {
				logger.V(1).Info("renewed port mapping", "protocol", m.Protocol, "externalPort", m.ExternalPort)
			}
		case mapping.Replace:
			err = r.UPnP.Delete(ctx, act.Existing.Protocol, act.Existing.ExternalPort)
			if errors.Is(err, upnp.ErrNoSuchEntry) {
				err = nil
			}
			if err == nil {
				err = r.UPnP.Add(ctx, m)
			}
			if err == nil {
				logger.Info("replaced port mapping", "protocol", m.Protocol, "externalPort", m.ExternalPort,
					"from", act.Existing.InternalClient, "to", m.InternalClient, "description", m.Description)
			}
		case mapping.Delete:
			m = *act.Existing
			err = r.UPnP.Delete(ctx, m.Protocol, m.ExternalPort)
			if errors.Is(err, upnp.ErrNoSuchEntry) {
				err = nil
			}
			if err == nil {
				logger.Info("deleted port mapping", "protocol", m.Protocol, "externalPort", m.ExternalPort)
			}
		}
		k := mappingKey(m.Protocol, m.ExternalPort)
		switch {
		case err == nil && act.Kind != mapping.Delete:
			nowSeen[k] = true
		case err == nil:
		case errors.Is(err, upnp.ErrUnreachable):
			logger.V(1).Info("router unreachable mid-pass", "error", err.Error())
			r.remember(svc.UID, nowSeen)
			return true
		case errors.Is(err, upnp.ErrConflict):
			r.conflict(svc, m, nil)
		default:
			r.Recorder.Eventf(svc, corev1.EventTypeWarning, "MappingFailed", "%s %s/%d: %v", act.Kind, m.Protocol, m.ExternalPort, err)
		}
	}
	r.remember(svc.UID, nowSeen)
	return false
}

func (r *ServiceReconciler) conflict(svc *corev1.Service, m upnp.PortMapping, existing *upnp.PortMapping) {
	r.Metrics.PortConflicts.Inc()
	holder := "another client"
	if existing != nil {
		holder = fmt.Sprintf("%s (%q)", existing.InternalClient, existing.Description)
	}
	r.Recorder.Eventf(svc, corev1.EventTypeWarning, "PortConflict", "router port %s/%d is already mapped to %s", m.Protocol, m.ExternalPort, holder)
}

func (r *ServiceReconciler) remember(uid types.UID, seen map[string]bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.seen == nil {
		r.seen = map[types.UID]map[string]bool{}
	}
	r.seen[uid] = seen
}

func (r *ServiceReconciler) forget(key types.NamespacedName, uid types.UID) {
	r.Metrics.DesiredMappings.DeleteLabelValues(key.Namespace, key.Name)
	if uid == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.seen, uid)
	delete(r.waiting, uid)
}

// cleanup deletes svc's mappings and then its finalizer. During deletion
// it gives up after FinalizerTimeout so a dead router never blocks it.
func (r *ServiceReconciler) cleanup(ctx context.Context, logger logr.Logger, svc *corev1.Service) (ctrl.Result, string, error) {
	actual, err := r.UPnP.List(ctx)
	failed := err != nil
	if !failed {
		failed = r.apply(ctx, logger, svc, nil, actual)
	}
	if failed {
		deleting := !svc.DeletionTimestamp.IsZero()
		if !deleting || r.Clock.Since(svc.DeletionTimestamp.Time) < r.FinalizerTimeout {
			return ctrl.Result{RequeueAfter: r.RetryInterval}, resultUnreachable, nil
		}
		r.Recorder.Eventf(svc, corev1.EventTypeWarning, "OrphanedMappings",
			"router unreachable for %s; removing finalizer, port mappings for this Service may remain on the router", r.FinalizerTimeout)
	}
	controllerutil.RemoveFinalizer(svc, Finalizer)
	if err := r.K8s.Update(ctx, svc); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, "", fmt.Errorf("remove finalizer: %w", err)
	}
	r.forget(client.ObjectKeyFromObject(svc), svc.UID)
	return ctrl.Result{}, resultCleanup, nil
}

// removeLegacyMetadata strips the Python controller's kopf finalizer and
// annotations (FR-SVC-10).
func (r *ServiceReconciler) removeLegacyMetadata(ctx context.Context, svc *corev1.Service) error {
	changed := controllerutil.RemoveFinalizer(svc, LegacyFinalizer)
	for _, a := range legacyAnnotations {
		if _, ok := svc.Annotations[a]; ok {
			delete(svc.Annotations, a)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	if err := r.K8s.Update(ctx, svc); err != nil {
		return fmt.Errorf("remove legacy kopf metadata: %w", err)
	}
	return nil
}

// SetupWithManager registers the reconciler for Services and resync events.
func (r *ServiceReconciler) SetupWithManager(mgr ctrl.Manager, resync *Resync, opts controller.Options) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("service").
		For(&corev1.Service{}, builder.WithPredicates(predicate.NewPredicateFuncs(relevant))).
		WatchesRawSource(resync.Source()).
		WithOptions(opts).
		Complete(r)
}
