// Package chart tests the Helm chart. It needs helm (and kubeconform for
// the schema test) on PATH; tests skip otherwise.
package chart

import (
	"bytes"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

var update = flag.Bool("update", false, "rewrite golden files")

const chartDir = "../../helm"

func helm(t *testing.T, args ...string) string {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	var out, stderr bytes.Buffer
	cmd := exec.CommandContext(t.Context(), "helm", args...)
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("helm %v: %v\n%s", args, err, stderr.String())
	}
	return out.String()
}

func render(t *testing.T, extra ...string) string {
	t.Helper()
	return helm(t, append([]string{"template", "upnp-nat-controller", chartDir, "--namespace", "upnp"}, extra...)...)
}

func docs(manifest string) []string {
	var out []string
	for _, d := range strings.Split(manifest, "\n---\n") {
		if strings.TrimSpace(d) != "" && strings.Contains(d, "kind:") {
			out = append(out, d)
		}
	}
	return out
}

func find(t *testing.T, manifest, kind string, into any) bool {
	t.Helper()
	for _, d := range docs(manifest) {
		var meta struct{ Kind string }
		if err := yaml.Unmarshal([]byte(d), &meta); err != nil {
			t.Fatal(err)
		}
		if meta.Kind == kind {
			if err := yaml.Unmarshal([]byte(d), into); err != nil {
				t.Fatal(err)
			}
			return true
		}
	}
	return false
}

func TestChart_Lint(t *testing.T) {
	helm(t, "lint", chartDir, "--strict")
}

func TestChart_RBACGolden(t *testing.T) {
	got := helm(t, "template", "upnp-nat-controller", chartDir, "--namespace", "upnp", "--show-only", "templates/rbac.yaml")
	golden := filepath.Join("testdata", "rbac.golden.yaml")
	if *update {
		if err := os.WriteFile(golden, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if norm(got) != norm(string(want)) {
		t.Fatalf("rendered RBAC differs from %s (run with -update after review):\n%s", golden, got)
	}
}

// norm makes the comparison insensitive to YAML flow vs block style.
func norm(s string) string {
	var out []string
	for _, d := range docs(s) {
		var v any
		if err := yaml.Unmarshal([]byte(d), &v); err != nil {
			return "unparseable: " + err.Error()
		}
		b, _ := yaml.Marshal(v)
		out = append(out, string(b))
	}
	return strings.Join(out, "---\n")
}

func TestChart_Deployment(t *testing.T) {
	var d appsv1.Deployment
	if !find(t, render(t), "Deployment", &d) {
		t.Fatal("no Deployment")
	}
	spec := d.Spec.Template.Spec
	if !spec.HostNetwork || spec.DNSPolicy != corev1.DNSClusterFirstWithHostNet {
		t.Errorf("hostNetwork=%v dnsPolicy=%s", spec.HostNetwork, spec.DNSPolicy)
	}
	if d.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		// hostNetwork defaults hostPort to containerPort, so a surge pod never fits on the node.
		t.Errorf("strategy %q, want Recreate", d.Spec.Strategy.Type)
	}
	if *d.Spec.Replicas != 1 {
		t.Errorf("replicas %d", *d.Spec.Replicas)
	}
	c := spec.Containers[0]
	if c.Image != "ghcr.io/nashant/upnp-nat-controller:0.1.0" {
		t.Errorf("image %s, want appVersion tag", c.Image)
	}
	for _, a := range []string{"--leader-elect=true", "--resync-interval=30s", "--lease-duration=3600", "--soap-timeout=5s",
		"--finalizer-timeout=10m", "--metrics-bind-address=:8080", "--health-probe-bind-address=:8081",
		"--description-prefix=unc/", "--rate-limit=5", "--rate-burst=10"} {
		if !slices.Contains(c.Args, a) {
			t.Errorf("args %v missing %s", c.Args, a)
		}
	}
	if slices.ContainsFunc(c.Args, func(a string) bool { return strings.HasPrefix(a, "--igd-url") }) {
		t.Error("--igd-url set by default")
	}
	if p := c.LivenessProbe; p == nil || p.HTTPGet.Path != "/healthz" || p.HTTPGet.Port.String() != "health" {
		t.Errorf("liveness %+v", p)
	}
	if p := c.ReadinessProbe; p == nil || p.HTTPGet.Path != "/readyz" {
		t.Errorf("readiness %+v", p)
	}
	if p := c.StartupProbe; p == nil || p.FailureThreshold != 30 || p.PeriodSeconds != 10 {
		t.Errorf("startup %+v", p)
	}
	if r := c.Resources.Requests; r.Cpu().String() != "10m" || r.Memory().String() != "64Mi" {
		t.Errorf("requests %v", r)
	}
	sc := c.SecurityContext
	if sc == nil || !*sc.ReadOnlyRootFilesystem || !*sc.RunAsNonRoot || *sc.AllowPrivilegeEscalation || len(sc.Capabilities.Drop) == 0 {
		t.Errorf("securityContext %+v", sc)
	}
}

func TestChart_IGDURLValue(t *testing.T) {
	var d appsv1.Deployment
	find(t, render(t, "--set", "controller.igdURL=http://192.168.1.1:2189/rootDesc.xml"), "Deployment", &d)
	if !slices.Contains(d.Spec.Template.Spec.Containers[0].Args, "--igd-url=http://192.168.1.1:2189/rootDesc.xml") {
		t.Fatalf("args %v", d.Spec.Template.Spec.Containers[0].Args)
	}
}

func TestChart_MetricsServiceAndServiceMonitor(t *testing.T) {
	m := render(t)
	var svc corev1.Service
	if !find(t, m, "Service", &svc) || svc.Spec.Ports[0].Port != 8080 {
		t.Fatalf("metrics Service %+v", svc.Spec)
	}
	if strings.Contains(m, "kind: ServiceMonitor") {
		t.Fatal("ServiceMonitor rendered by default")
	}
	if !strings.Contains(render(t, "--set", "metrics.serviceMonitor.enabled=true"), "kind: ServiceMonitor") {
		t.Fatal("ServiceMonitor not rendered when enabled")
	}
}

func TestChart_CRDIsNewGroupOnly(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join(chartDir, "crds"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !slices.Equal(names, []string{"gateway.nashes.uk_internetgatewaydevices.yaml"}) {
		t.Fatalf("crds %v", names)
	}
}

func TestChart_Kubeconform(t *testing.T) {
	if _, err := exec.LookPath("kubeconform"); err != nil {
		t.Skip("kubeconform not on PATH")
	}
	m := render(t, "--set", "metrics.serviceMonitor.enabled=true")
	cmd := exec.CommandContext(t.Context(), "kubeconform", "-strict", "-summary", "-ignore-missing-schemas", "-")
	cmd.Stdin = strings.NewReader(m)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("kubeconform: %v\n%s", err, out)
	}
}
