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
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/source"

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
	resultSuccess       = "success"
	resultUnreachable   = "router_unreachable"
	resultWaiting       = "waiting"
	resultInvalid       = "invalid"
	resultSkipped       = "skipped"
	resultCleanup       = "cleanup"
	resultCleanupFailed = "cleanup_failed"
	resultError         = "error"
)

// ServiceReconciler keeps the router's port mappings in line with the
// annotations of each managed LoadBalancer Service. Every successful pass
// requeues after ResyncInterval, so mappings lost on the router side are
// restored without any Service event.
type ServiceReconciler struct {
	K8s      client.Client
	UPnP     upnp.Client
	Recorder events.EventRecorder
	Clock    clock.PassiveClock
	Metrics  *metrics.Metrics
	// OnPass, if set, is called after every reconcile that ran to completion.
	OnPass func()
	// Names builds and recognises mapping descriptions; zero means the defaults.
	Names mapping.Descriptions

	ResyncInterval   time.Duration // requeue after a good pass (30s)
	RetryInterval    time.Duration // requeue while the router is unreachable (10s)
	IPWaitInterval   time.Duration // requeue while waiting for an LB IP (5s)
	DefaultLease     uint32        // seconds (3600)
	FinalizerTimeout time.Duration // give up on cleanup during deletion (10m)

	mu sync.Mutex
	// waiting holds Services that already got a WaitingForIP event.
	waiting map[types.UID]string
	// seen holds the mappings each Service had on the router at its last
	// pass, to tell a drift repair from a first add.
	seen map[types.UID]map[portKey]bool
}

type portKey struct {
	proto corev1.Protocol
	port  uint16
}

// Reconcile implements reconcile.Reconciler.
func (r *ServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if req.NamespacedName == HeartbeatKey {
		if r.OnPass != nil {
			r.OnPass()
		}
		return ctrl.Result{}, nil
	}
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
		r.Recorder.Eventf(&svc, nil, corev1.EventTypeWarning, "NotLoadBalancer", "Reconcile",
			"Service has %s annotations but type %s; only LoadBalancer Services are mapped", "advertise.upnp", svc.Spec.Type)
	}
	if !managed || !isLB || !svc.DeletionTimestamp.IsZero() {
		// A Service deleted while it still carries the kopf finalizer gets
		// the same cleanup as ours: it may own Python controller mappings.
		if !controllerutil.ContainsFinalizer(&svc, Finalizer) && !controllerutil.ContainsFinalizer(&svc, LegacyFinalizer) {
			result := ""
			if managed {
				result = resultSkipped
			}
			return ctrl.Result{}, result, nil
		}
		return r.cleanup(ctx, logger, &svc)
	}

	spec, err := annotations.Parse(svc.Annotations, svc.Spec.Ports, r.DefaultLease)
	if err == nil {
		err = spec.ValidateLease(clampUint32(int64(2 * r.ResyncInterval / time.Second)))
	}
	if err != nil {
		r.Recorder.Eventf(&svc, nil, corev1.EventTypeWarning, "InvalidAnnotations", "Reconcile", "%v", err)
		return ctrl.Result{}, resultInvalid, nil
	}

	ip, ok := r.ingressIP(&svc)
	if !ok {
		// Without an IPv4 target, mappings made for the previous IP must go:
		// the LB pool may hand that IP to another Service.
		if controllerutil.ContainsFinalizer(&svc, Finalizer) {
			if actual, err := r.UPnP.List(ctx); err != nil || r.apply(ctx, logger, &svc, nil, actual).unreachable {
				return ctrl.Result{RequeueAfter: r.RetryInterval}, resultUnreachable, nil
			}
		}
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
	if r.apply(ctx, logger, &svc, desired, actual).unreachable {
		return ctrl.Result{RequeueAfter: r.RetryInterval}, resultUnreachable, nil
	}
	return ctrl.Result{RequeueAfter: r.ResyncInterval}, resultSuccess, nil
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
	reason, typ, msg := "WaitingForIP", corev1.EventTypeNormal, "waiting for a LoadBalancer IP"
	if len(ingress) > 0 {
		reason, typ, msg = "NoIPv4Ingress", corev1.EventTypeWarning, "LoadBalancer ingress has no IPv4 address; UPnP mappings need one"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.waiting == nil {
		r.waiting = map[types.UID]string{}
	}
	if r.waiting[svc.UID] != reason {
		r.waiting[svc.UID] = reason
		r.Recorder.Eventf(svc, nil, typ, reason, "Reconcile", "%s", msg)
	}
	return "", false
}

// applyResult reports how a pass over the plan went.
type applyResult struct {
	// unreachable: the router stopped answering; remaining actions were skipped.
	unreachable bool
	// failed: at least one action failed for another reason (fault, conflict).
	failed bool
}

// apply executes the plan for svc.
func (r *ServiceReconciler) apply(ctx context.Context, logger logr.Logger, svc *corev1.Service, desired []mapping.Desired, actual []upnp.PortMapping) applyResult {
	var res applyResult
	owner := mapping.Owner{Namespace: svc.Namespace, Name: svc.Name}
	r.mu.Lock()
	prevSeen := r.seen[svc.UID]
	r.mu.Unlock()
	onRouter := make(map[portKey]bool, len(actual))
	for _, a := range actual {
		onRouter[portKey{a.Protocol, a.ExternalPort}] = true
	}
	nowSeen := map[portKey]bool{}
	for _, d := range desired {
		if k := (portKey{d.Protocol, d.ExternalPort}); onRouter[k] {
			nowSeen[k] = true
		}
	}

	for _, act := range names(r.Names).Plan(owner, desired, actual, 2*r.ResyncInterval) {
		var err error
		m := act.Mapping
		switch act.Kind {
		case mapping.Conflict:
			r.conflict(svc, m, act.Existing)
			res.failed = true
			continue
		case mapping.Add, mapping.Renew:
			err = r.UPnP.Add(ctx, m)
			if err == nil && act.Kind == mapping.Add && prevSeen[portKey{m.Protocol, m.ExternalPort}] {
				r.Metrics.DriftRepairs.Inc()
				logger.Info("re-added missing port mapping", "protocol", m.Protocol, "externalPort", m.ExternalPort)
			} else if err == nil && act.Kind == mapping.Add {
				logger.Info("added port mapping", "protocol", m.Protocol, "externalPort", m.ExternalPort, "internalClient", m.InternalClient, "internalPort", m.InternalPort)
			} else if err == nil {
				logger.V(1).Info("renewed port mapping", "protocol", m.Protocol, "externalPort", m.ExternalPort)
			}
		case mapping.Replace:
			err = r.deleteIfPresent(ctx, act.Existing.Protocol, act.Existing.ExternalPort)
			if err == nil {
				err = r.UPnP.Add(ctx, m)
			}
			if err == nil {
				logger.Info("replaced port mapping", "protocol", m.Protocol, "externalPort", m.ExternalPort,
					"from", act.Existing.InternalClient, "to", m.InternalClient, "description", m.Description)
			}
		case mapping.Delete:
			m = *act.Existing
			err = r.deleteIfPresent(ctx, m.Protocol, m.ExternalPort)
			if err == nil {
				logger.Info("deleted port mapping", "protocol", m.Protocol, "externalPort", m.ExternalPort)
			}
		}
		k := portKey{m.Protocol, m.ExternalPort}
		switch {
		case err == nil && act.Kind != mapping.Delete:
			nowSeen[k] = true
		case err == nil:
		case errors.Is(err, upnp.ErrUnreachable):
			logger.V(1).Info("router unreachable mid-pass", "error", err.Error())
			r.remember(svc.UID, nowSeen)
			res.unreachable = true
			return res
		case errors.Is(err, upnp.ErrConflict):
			r.conflict(svc, m, nil)
			res.failed = true
		default:
			r.Recorder.Eventf(svc, nil, corev1.EventTypeWarning, "MappingFailed", "Reconcile", "%s %s/%d: %v", act.Kind, m.Protocol, m.ExternalPort, err)
			res.failed = true
		}
	}
	r.remember(svc.UID, nowSeen)
	return res
}

// deleteIfPresent deletes a mapping; one already gone is not an error.
func (r *ServiceReconciler) deleteIfPresent(ctx context.Context, proto corev1.Protocol, port uint16) error {
	if err := r.UPnP.Delete(ctx, proto, port); err != nil && !errors.Is(err, upnp.ErrNoSuchEntry) {
		return err
	}
	return nil
}

func (r *ServiceReconciler) conflict(svc *corev1.Service, m upnp.PortMapping, existing *upnp.PortMapping) {
	r.Metrics.PortConflicts.Inc()
	holder := "another client"
	if existing != nil {
		holder = fmt.Sprintf("%s (%q)", existing.InternalClient, existing.Description)
	}
	r.Recorder.Eventf(svc, nil, corev1.EventTypeWarning, "PortConflict", "Reconcile", "router port %s/%d is already mapped to %s", m.Protocol, m.ExternalPort, holder)
}

func (r *ServiceReconciler) remember(uid types.UID, seen map[portKey]bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.seen == nil {
		r.seen = map[types.UID]map[portKey]bool{}
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
	result := resultUnreachable
	failed := err != nil
	if !failed {
		res := r.apply(ctx, logger, svc, nil, actual)
		failed = res.unreachable || res.failed
		if !res.unreachable {
			result = resultCleanupFailed
		}
	}
	if failed {
		deleting := !svc.DeletionTimestamp.IsZero()
		if !deleting || r.Clock.Since(svc.DeletionTimestamp.Time) < r.FinalizerTimeout {
			return ctrl.Result{RequeueAfter: r.RetryInterval}, result, nil
		}
		r.Recorder.Eventf(svc, nil, corev1.EventTypeWarning, "OrphanedMappings", "Reconcile",
			"could not delete this Service's port mappings within %s; removing finalizer, they may remain on the router", r.FinalizerTimeout)
	}
	controllerutil.RemoveFinalizer(svc, Finalizer)
	controllerutil.RemoveFinalizer(svc, LegacyFinalizer)
	if err := r.K8s.Update(ctx, svc); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, "", fmt.Errorf("remove finalizer: %w", err)
	}
	r.forget(client.ObjectKeyFromObject(svc), svc.UID)
	return ctrl.Result{}, resultCleanup, nil
}

// removeLegacyMetadata strips the Python controller's kopf finalizer and
// annotations. On a Service being deleted the finalizer is
// kept until cleanup has removed its mappings.
func (r *ServiceReconciler) removeLegacyMetadata(ctx context.Context, svc *corev1.Service) error {
	changed := svc.DeletionTimestamp.IsZero() && controllerutil.RemoveFinalizer(svc, LegacyFinalizer)
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

// SetupWithManager registers the reconciler for Services plus extra
// sources (resync, heartbeat).
func (r *ServiceReconciler) SetupWithManager(mgr ctrl.Manager, opts controller.Options, sources ...source.Source) error {
	b := ctrl.NewControllerManagedBy(mgr).
		Named("service").
		For(&corev1.Service{}, builder.WithPredicates(predicate.NewPredicateFuncs(relevant))).
		WithOptions(opts)
	for _, src := range sources {
		b = b.WatchesRawSource(src)
	}
	return b.Complete(r)
}

func names(d mapping.Descriptions) mapping.Descriptions {
	if d.Prefix == "" {
		return mapping.DefaultDescriptions
	}
	return d
}
