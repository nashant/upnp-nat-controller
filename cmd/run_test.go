package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	gatewayv1alpha1 "github.com/nashant/upnp-nat-controller/api/v1alpha1"
	"github.com/nashant/upnp-nat-controller/internal/annotations"
	"github.com/nashant/upnp-nat-controller/internal/upnp/fakeigd"
)

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func TestRun_WiresControllersProbesAndMetrics(t *testing.T) { // M8 wiring, NFR-OPS-1/2/3
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("needs envtest; run with make test")
	}
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "helm", "crds")}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = gatewayv1alpha1.AddToScheme(scheme)
	k8s, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}

	f := fakeigd.New(t)
	u, _ := url.Parse(f.URL())
	probe, metricsAddr := freePort(t), freePort(t)
	o, err := parseFlags([]string{"--leader-elect=false", "--resync-interval=500ms",
		"--health-probe-bind-address=" + probe, "--metrics-bind-address=" + metricsAddr, "--igd-url=" + u.String()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, cfg, o) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("run: %v", err)
		}
	})

	eventually := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	httpOK := func(path string, addr string) func() bool {
		return func() bool {
			resp, err := http.Get(fmt.Sprintf("http://%s%s", addr, path))
			if err != nil {
				return false
			}
			resp.Body.Close()
			return resp.StatusCode == http.StatusOK
		}
	}
	eventually("/readyz", httpOK("/readyz", probe))
	eventually("/healthz", httpOK("/healthz", probe))
	eventually("/metrics", httpOK("/metrics", metricsAddr))
	eventually("IGD CR", func() bool {
		var igd gatewayv1alpha1.InternetGatewayDevice
		return k8s.Get(ctx, client.ObjectKey{Name: "default"}, &igd) == nil && igd.Status.ExternalIP != ""
	})

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web",
			Annotations: map[string]string{annotations.TCPEnabled: "true", annotations.TCPPorts: "443"}},
		Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer, Ports: []corev1.ServicePort{{Port: 443, Protocol: corev1.ProtocolTCP}}},
	}
	if err := k8s.Create(ctx, svc); err != nil {
		t.Fatal(err)
	}
	svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "172.16.1.2"}}
	if err := k8s.Status().Update(ctx, svc); err != nil {
		t.Fatal(err)
	}
	eventually("Service mapped", func() bool { return len(f.Mappings()) == 1 })
	f.Restart()
	eventually("mapping restored after router restart", func() bool { return len(f.Mappings()) == 1 })
}
