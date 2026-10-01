package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

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

func startEnv(t *testing.T) (*envtest.Environment, *rest.Config, client.Client) {
	t.Helper()
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
	return env, cfg, k8s
}

func TestRun_WiresControllersProbesAndMetrics(t *testing.T) { // M8 wiring, NFR-OPS-1/2/3
	_, cfg, k8s := startEnv(t)
	runAndCheck(t, cfg, k8s, "--leader-elect=false")
}

// TestRun_ChartRBACIsSufficient runs the controller as the chart's
// ServiceAccount with only the chart's RBAC (D12).
func TestRun_ChartRBACIsSufficient(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	env, _, k8s := startEnv(t)
	ctx := context.Background()
	if err := k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "upnp"}}); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("helm", "template", "upnp-nat-controller", "../helm", "--namespace", "upnp", "--show-only", "templates/rbac.yaml").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range strings.Split(string(out), "\n---\n") {
		if !strings.Contains(doc, "kind:") {
			continue
		}
		obj := &unstructured.Unstructured{}
		if err := yaml.Unmarshal([]byte(doc), &obj.Object); err != nil {
			t.Fatal(err)
		}
		if err := k8s.Create(ctx, obj); err != nil {
			t.Fatalf("apply %s: %v", obj.GetKind(), err)
		}
	}
	sa, err := env.AddUser(envtest.User{Name: "system:serviceaccount:upnp:upnp-nat-controller", Groups: []string{"system:serviceaccounts", "system:authenticated"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	runAndCheck(t, sa.Config(), k8s, "--leader-elect=true", "--leader-election-namespace=upnp")
}

func runAndCheck(t *testing.T, cfg *rest.Config, k8s client.Client, extra ...string) {
	t.Helper()
	f := fakeigd.New(t)
	u, _ := url.Parse(f.URL())
	probe, metricsAddr := freePort(t), freePort(t)
	o, err := parseFlags(append([]string{"--resync-interval=500ms",
		"--health-probe-bind-address=" + probe, "--metrics-bind-address=" + metricsAddr, "--igd-url=" + u.String()}, extra...))
	if err != nil {
		t.Fatal(err)
	}
	o.skipNameValidation = true
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, cfg, o, prometheus.NewRegistry()) }()
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
	eventually("finalizer added", func() bool {
		var got corev1.Service
		return k8s.Get(ctx, client.ObjectKeyFromObject(svc), &got) == nil && slices.Contains(got.Finalizers, "upnp.nashes.uk/port-mappings")
	})
	if err := k8s.Delete(ctx, svc); err != nil {
		t.Fatal(err)
	}
	eventually("Service deleted and mapping removed", func() bool {
		return apierrors.IsNotFound(k8s.Get(ctx, client.ObjectKeyFromObject(svc), &corev1.Service{})) && len(f.Mappings()) == 0
	})
}
