// Command manager runs the UPnP NAT controller.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/time/rate"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	gatewayv1alpha1 "github.com/nashant/upnp-nat-controller/api/v1alpha1"
	ctl "github.com/nashant/upnp-nat-controller/internal/controller"
	"github.com/nashant/upnp-nat-controller/internal/health"
	"github.com/nashant/upnp-nat-controller/internal/metrics"
	"github.com/nashant/upnp-nat-controller/internal/upnp"
)

// Options are the command-line options.
type Options struct {
	MetricsAddr      string
	ProbeAddr        string
	LeaderElect      bool
	LeaderElectionNS string
	IGDURL           *url.URL
	ResyncInterval   time.Duration
	LeaseDuration    uint32
	SOAPTimeout      time.Duration
	FinalizerTimeout time.Duration
	RateLimit        float64
	RateBurst        int
	Zap              zap.Options

	// skipNameValidation lets tests start the manager more than once per process.
	skipNameValidation bool
}

func parseFlags(args []string) (Options, error) {
	var (
		o      Options
		igdURL string
		lease  int64
	)
	fs := flag.NewFlagSet("manager", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.MetricsAddr, "metrics-bind-address", ":8080", "Address the metrics endpoint binds to.")
	fs.StringVar(&o.ProbeAddr, "health-probe-bind-address", ":8081", "Address the health probe endpoint binds to.")
	fs.BoolVar(&o.LeaderElect, "leader-elect", true, "Enable leader election.")
	fs.StringVar(&o.LeaderElectionNS, "leader-election-namespace", "", "Namespace for the leader election lease (default: the pod's namespace).")
	fs.StringVar(&igdURL, "igd-url", "", "Root device description URL of the router; skips SSDP discovery.")
	fs.DurationVar(&o.ResyncInterval, "resync-interval", 30*time.Second, "How often every managed Service is reconciled.")
	fs.Int64Var(&lease, "lease-duration", 3600, "Default port mapping lease in seconds (0 = permanent).")
	fs.DurationVar(&o.SOAPTimeout, "soap-timeout", 5*time.Second, "Timeout for each router request.")
	fs.DurationVar(&o.FinalizerTimeout, "finalizer-timeout", 10*time.Minute, "How long a deleted Service waits for an unreachable router before its finalizer is removed.")
	fs.Float64Var(&o.RateLimit, "rate-limit", 5, "Router requests per second.")
	fs.IntVar(&o.RateBurst, "rate-burst", 10, "Router request burst.")
	o.Zap.BindFlags(fs)
	if err := fs.Parse(args); err != nil {
		return Options{}, err
	}

	var errs []error
	if igdURL != "" {
		u, err := url.Parse(igdURL)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("--igd-url: %w", err))
		case u.Scheme != "http" && u.Scheme != "https" || u.Host == "":
			errs = append(errs, fmt.Errorf("--igd-url: want an http(s) URL, got %q", igdURL))
		default:
			o.IGDURL = u
		}
	}
	if lease < 0 || lease > 1<<32-1 {
		errs = append(errs, fmt.Errorf("--lease-duration: %d out of range", lease))
	}
	o.LeaseDuration = uint32(lease)
	if o.ResyncInterval <= 0 {
		errs = append(errs, errors.New("--resync-interval must be positive"))
	}
	if o.SOAPTimeout <= 0 {
		errs = append(errs, errors.New("--soap-timeout must be positive"))
	}
	return o, errors.Join(errs...)
}

func main() {
	o, err := parseFlags(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&o.Zap)))
	if err := run(ctrl.SetupSignalHandler(), ctrl.GetConfigOrDie(), o, crmetrics.Registry); err != nil {
		ctrl.Log.Error(err, "manager exited")
		os.Exit(1)
	}
}

// run starts the manager; the controller's metrics are registered with reg.
func run(ctx context.Context, cfg *rest.Config, o Options, reg prometheus.Registerer) error {
	log := ctrl.Log.WithName("setup")
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}
	if err := gatewayv1alpha1.AddToScheme(scheme); err != nil {
		return err
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsserver.Options{BindAddress: o.MetricsAddr},
		HealthProbeBindAddress:  o.ProbeAddr,
		LeaderElection:          o.LeaderElect,
		LeaderElectionID:        "upnp-nat-controller.nashes.uk",
		LeaderElectionNamespace: o.LeaderElectionNS,
		// Mappings are left on the router at shutdown (NFR-OPS-5); release
		// the lease so a replacement takes over quickly.
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}

	m := metrics.New(reg)
	router := metrics.Instrument(upnp.New(upnp.Config{
		IGDURL:      o.IGDURL,
		SOAPTimeout: o.SOAPTimeout,
		Limiter:     rate.NewLimiter(rate.Limit(o.RateLimit), o.RateBurst),
	}), m)
	clk := clock.RealClock{}
	rec := mgr.GetEventRecorder("upnp-nat-controller")
	tracker := health.New(clk)
	resync := ctl.NewResync(mgr.GetClient())
	heartbeat := ctl.NewHeartbeat(clk, o.ResyncInterval)
	if err := mgr.Add(heartbeat); err != nil {
		return fmt.Errorf("add heartbeat: %w", err)
	}

	r := &ctl.ServiceReconciler{
		K8s: mgr.GetClient(), UPnP: router, Recorder: rec,
		Clock: clk, Metrics: m, OnPass: func() { tracker.Beat("service-reconciler", o.ResyncInterval) },
		ResyncInterval: o.ResyncInterval, RetryInterval: 10 * time.Second, IPWaitInterval: 5 * time.Second,
		DefaultLease: o.LeaseDuration, FinalizerTimeout: o.FinalizerTimeout,
	}
	if err := r.SetupWithManager(mgr, controller.Options{SkipNameValidation: &o.skipNameValidation}, resync.Source(), heartbeat.Source()); err != nil {
		return fmt.Errorf("set up Service controller: %w", err)
	}
	poller := &ctl.IGDPoller{
		K8s: mgr.GetClient(), UPnP: router, Recorder: rec,
		Clock: clk, Resync: resync, Metrics: m, Log: ctrl.Log.WithName("igd"),
		OnPass: func(next time.Duration) { tracker.Beat("igd-poller", next) },
	}
	if err := mgr.Add(poller); err != nil {
		return fmt.Errorf("add IGD poller: %w", err)
	}

	if err := mgr.AddHealthzCheck("control-loops", tracker.Check); err != nil {
		return err
	}
	if err := mgr.AddReadyzCheck("informers", func(req *http.Request) error {
		ctx, cancel := context.WithTimeout(req.Context(), time.Second)
		defer cancel()
		if !mgr.GetCache().WaitForCacheSync(ctx) {
			return errors.New("informer caches not synced")
		}
		return nil
	}); err != nil {
		return err
	}

	log.Info("starting manager", "resyncInterval", o.ResyncInterval, "igdURL", o.IGDURL)
	return mgr.Start(ctx)
}
