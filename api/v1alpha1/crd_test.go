package v1alpha1_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	gatewayv1alpha1 "github.com/nashant/upnp-nat-controller/api/v1alpha1"
)

var k8s client.Client

func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		// envtest tests run via `make test`; plain `go test` skips them.
		os.Exit(0)
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "helm", "crds")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		panic(err)
	}
	scheme := runtime.NewScheme()
	if err := gatewayv1alpha1.AddToScheme(scheme); err != nil {
		panic(err)
	}
	if k8s, err = client.New(cfg, client.Options{Scheme: scheme}); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = env.Stop()
	os.Exit(code)
}

func TestCRD_ValidatesPollingIntervalMin(t *testing.T) { // FR-IGD-1
	ctx := context.Background()
	bad := &gatewayv1alpha1.InternetGatewayDevice{
		ObjectMeta: metav1.ObjectMeta{Name: "too-fast"},
		Spec:       gatewayv1alpha1.InternetGatewayDeviceSpec{PollingInterval: 5},
	}
	err := k8s.Create(ctx, bad)
	if err == nil || !strings.Contains(err.Error(), "pollingInterval") {
		t.Fatalf("want pollingInterval validation error, got %v", err)
	}

	ok := &gatewayv1alpha1.InternetGatewayDevice{ObjectMeta: metav1.ObjectMeta{Name: "defaulted"}}
	if err := k8s.Create(ctx, ok); err != nil {
		t.Fatal(err)
	}
	if ok.Spec.PollingInterval != 30 {
		t.Fatalf("default pollingInterval = %d, want 30", ok.Spec.PollingInterval)
	}
	if ok.Namespace != "" {
		t.Fatalf("want cluster-scoped, got namespace %q", ok.Namespace)
	}
}

func TestCRD_StatusSubresource(t *testing.T) { // FR-IGD-2
	ctx := context.Background()
	igd := &gatewayv1alpha1.InternetGatewayDevice{ObjectMeta: metav1.ObjectMeta{Name: "status"}}
	if err := k8s.Create(ctx, igd); err != nil {
		t.Fatal(err)
	}
	now := metav1.Now()
	igd.Status = gatewayv1alpha1.InternetGatewayDeviceStatus{
		FriendlyName: "OPNsense UPnP IGD & PCP", ExternalIP: "81.2.69.142", Uptime: 10, LastSeen: &now,
		PortMappings: []gatewayv1alpha1.PortMappingStatus{{
			Protocol: "TCP", ExternalPort: 443, InternalPort: 443, InternalClient: "172.16.1.2", Enabled: true,
			Description: "upnp-nat-controller/traefik/public-traefik", LeaseDuration: 3600,
			ServiceRef: gatewayv1alpha1.ServiceRef{Namespace: "traefik", Name: "public-traefik"},
		}},
		Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Polled", LastTransitionTime: now}},
	}
	// A main-resource update must not write status.
	if err := k8s.Update(ctx, igd.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	var got gatewayv1alpha1.InternetGatewayDevice
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(igd), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.FriendlyName != "" {
		t.Fatal("status written through main resource: no status subresource")
	}
	igd.ResourceVersion = got.ResourceVersion
	if err := k8s.Status().Update(ctx, igd); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(igd), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.PortMappings[0].ServiceRef.Name != "public-traefik" || got.Status.Conditions[0].Type != "Ready" {
		t.Fatalf("status not stored: %+v", got.Status)
	}
}

func TestCRD_PortMappingProtocolEnum(t *testing.T) {
	ctx := context.Background()
	igd := &gatewayv1alpha1.InternetGatewayDevice{ObjectMeta: metav1.ObjectMeta{Name: "enum"}}
	if err := k8s.Create(ctx, igd); err != nil {
		t.Fatal(err)
	}
	igd.Status.PortMappings = []gatewayv1alpha1.PortMappingStatus{{Protocol: "SCTP", ExternalPort: 1, InternalPort: 1}}
	if err := k8s.Status().Update(ctx, igd); err == nil {
		t.Fatal("want protocol enum rejection")
	}
}
