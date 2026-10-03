package controller

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	gatewayv1alpha1 "github.com/nashant/upnp-nat-controller/api/v1alpha1"
)

var (
	k8s     client.Client
	restCfg *rest.Config
	scheme  = runtime.NewScheme()
	bg      = context.Background()
)

func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		// Controller tests need envtest; run them with `make test`.
		os.Exit(0)
	}
	_ = clientgoscheme.AddToScheme(scheme)
	_ = gatewayv1alpha1.AddToScheme(scheme)
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "helm", "crds")},
		ErrorIfCRDPathMissing: true,
	}
	var err error
	if restCfg, err = env.Start(); err != nil {
		panic(err)
	}
	if k8s, err = client.New(restCfg, client.Options{Scheme: scheme}); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = env.Stop()
	os.Exit(code)
}
