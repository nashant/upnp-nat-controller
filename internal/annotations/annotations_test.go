package annotations

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
)

const defLease = 3600

func svcPorts(ps ...corev1.ServicePort) []corev1.ServicePort { return ps }

func tcp(p int32) corev1.ServicePort {
	return corev1.ServicePort{Protocol: corev1.ProtocolTCP, Port: p}
}
func udp(p int32) corev1.ServicePort {
	return corev1.ServicePort{Protocol: corev1.ProtocolUDP, Port: p}
}

func TestParse_TCPEnabledSinglePort(t *testing.T) { // FR-ANN-1
	got, err := Parse(map[string]string{
		TCPEnabled: "true",
		TCPPorts:   "443",
	}, svcPorts(tcp(443)), defLease)
	if err != nil {
		t.Fatal(err)
	}
	want := Spec{LeaseSeconds: defLease, Mappings: []Desired{{Protocol: corev1.ProtocolTCP, ExternalPort: 443, InternalPort: 443}}}
	if d := cmp.Diff(want, got); d != "" {
		t.Fatalf("(-want +got)\n%s", d)
	}
}

func TestParse_UDPOnly(t *testing.T) { // D8
	got, err := Parse(map[string]string{
		UDPEnabled: "true",
		UDPPorts:   "51820",
	}, svcPorts(udp(51820)), defLease)
	if err != nil {
		t.Fatal(err)
	}
	want := []Desired{{Protocol: corev1.ProtocolUDP, ExternalPort: 51820, InternalPort: 51820}}
	if d := cmp.Diff(want, got.Mappings); d != "" {
		t.Fatalf("(-want +got)\n%s", d)
	}
}

func TestParse_BothProtocols(t *testing.T) {
	got, err := Parse(map[string]string{
		TCPEnabled: "true", TCPPorts: "443,32400",
		UDPEnabled: "true", UDPPorts: "443",
	}, svcPorts(tcp(443), tcp(32400), udp(443)), defLease)
	if err != nil {
		t.Fatal(err)
	}
	want := []Desired{
		{Protocol: corev1.ProtocolTCP, ExternalPort: 443, InternalPort: 443},
		{Protocol: corev1.ProtocolTCP, ExternalPort: 32400, InternalPort: 32400},
		{Protocol: corev1.ProtocolUDP, ExternalPort: 443, InternalPort: 443},
	}
	if d := cmp.Diff(want, got.Mappings); d != "" {
		t.Fatalf("(-want +got)\n%s", d)
	}
}

func TestParse_DisabledOrMissing_ReturnsNone(t *testing.T) {
	cases := map[string]map[string]string{
		"nil":              nil,
		"no annotations":   {},
		"disabled":         {TCPEnabled: "false", TCPPorts: "443"},
		"ports only":       {TCPPorts: "443"},
		"enabled no ports": {TCPEnabled: "true"},
	}
	for name, ann := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := Parse(ann, svcPorts(tcp(443)), defLease)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Mappings) != 0 {
				t.Fatalf("want no mappings, got %v", got.Mappings)
			}
		})
	}
}

func TestParse_EnabledNotExactlyTrue(t *testing.T) { // FR-ANN-1
	for v, want := range map[string]bool{
		"true": true, "TRUE": true, "True": true,
		"yes": false, "1": false, "true ": false, "": false, "truee": false,
	} {
		if got := IsManaged(map[string]string{TCPEnabled: v}); got != want {
			t.Errorf("enabled=%q: IsManaged=%v, want %v", v, got, want)
		}
	}
}

func TestParse_PortsWhitespaceAndOrder_SortedDeduped(t *testing.T) { // FR-ANN-2, FR-ANN-6
	got, err := Parse(map[string]string{
		TCPEnabled: "true", TCPPorts: " 32400 , 443,443 ,",
	}, svcPorts(tcp(443), tcp(32400)), defLease)
	if err != nil {
		t.Fatal(err)
	}
	want := []Desired{
		{Protocol: corev1.ProtocolTCP, ExternalPort: 443, InternalPort: 443},
		{Protocol: corev1.ProtocolTCP, ExternalPort: 32400, InternalPort: 32400},
	}
	if d := cmp.Diff(want, got.Mappings); d != "" {
		t.Fatalf("(-want +got)\n%s", d)
	}
}

func TestParse_ExtIntSyntax(t *testing.T) { // FR-ANN-2
	got, err := Parse(map[string]string{
		TCPEnabled: "true", TCPPorts: "8443:443",
	}, svcPorts(tcp(443)), defLease)
	if err != nil {
		t.Fatal(err)
	}
	want := []Desired{{Protocol: corev1.ProtocolTCP, ExternalPort: 8443, InternalPort: 443}}
	if d := cmp.Diff(want, got.Mappings); d != "" {
		t.Fatalf("(-want +got)\n%s", d)
	}
}

func TestParse_PortOutOfRange(t *testing.T) { // FR-ANN-2
	for _, p := range []string{"0", "65536", "-1", "443:0", "70000:443"} {
		if _, err := Parse(map[string]string{TCPEnabled: "true", TCPPorts: p}, svcPorts(tcp(443)), defLease); err == nil {
			t.Errorf("ports=%q: want error", p)
		}
	}
}

func TestParse_NonNumeric(t *testing.T) { // D10
	for _, p := range []string{"https", "443a", "a:443", "443:b", "1:2:3", ":443", "443:"} {
		if _, err := Parse(map[string]string{TCPEnabled: "true", TCPPorts: p}, svcPorts(tcp(443)), defLease); err == nil {
			t.Errorf("ports=%q: want error", p)
		}
	}
}

func TestParse_DuplicateExternalPort_Error(t *testing.T) { // FR-ANN-3
	_, err := Parse(map[string]string{
		TCPEnabled: "true", TCPPorts: "443,443:8443",
	}, svcPorts(tcp(443), tcp(8443)), defLease)
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("want duplicate error, got %v", err)
	}
}

func TestParse_SameExternalPortDifferentProtocol_OK(t *testing.T) { // FR-ANN-3
	if _, err := Parse(map[string]string{
		TCPEnabled: "true", TCPPorts: "443",
		UDPEnabled: "true", UDPPorts: "443",
	}, svcPorts(tcp(443), udp(443)), defLease); err != nil {
		t.Fatal(err)
	}
}

func TestParse_InternalPortNotOnService_Error(t *testing.T) { // FR-ANN-4
	cases := map[string]struct {
		ann   map[string]string
		ports []corev1.ServicePort
	}{
		"missing port":   {map[string]string{TCPEnabled: "true", TCPPorts: "80"}, svcPorts(tcp(443))},
		"wrong protocol": {map[string]string{UDPEnabled: "true", UDPPorts: "443"}, svcPorts(tcp(443))},
		"ext:int miss":   {map[string]string{TCPEnabled: "true", TCPPorts: "443:8443"}, svcPorts(tcp(443))},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse(c.ann, c.ports, defLease)
			if err == nil || !strings.Contains(err.Error(), "not a service port") {
				t.Fatalf("want not-a-service-port error, got %v", err)
			}
		})
	}
}

func TestParse_EmptyProtocolServicePortIsTCP(t *testing.T) { // FR-ANN-4: API default protocol is TCP
	if _, err := Parse(map[string]string{TCPEnabled: "true", TCPPorts: "443"},
		svcPorts(corev1.ServicePort{Port: 443}), defLease); err != nil {
		t.Fatal(err)
	}
}

func TestParse_MultipleErrors_Aggregated(t *testing.T) { // FR-ANN-6
	_, err := Parse(map[string]string{
		TCPEnabled: "true", TCPPorts: "abc,70000,80",
		UDPEnabled: "true", UDPPorts: "x",
		LeaseSeconds: "-5",
	}, svcPorts(tcp(443)), defLease)
	if err == nil {
		t.Fatal("want error")
	}
	for _, s := range []string{`"abc"`, `"70000"`, "80", `"x"`, "lease"} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("error %q does not mention %s", err, s)
		}
	}
}

func TestParse_LeaseAnnotation_DefaultAndOverride(t *testing.T) { // FR-ANN-5
	base := map[string]string{TCPEnabled: "true", TCPPorts: "443"}
	got, err := Parse(base, svcPorts(tcp(443)), defLease)
	if err != nil || got.LeaseSeconds != defLease {
		t.Fatalf("default: got %d, %v", got.LeaseSeconds, err)
	}
	for v, want := range map[string]uint32{"0": 0, "120": 120, "604800": 604800} {
		ann := map[string]string{TCPEnabled: "true", TCPPorts: "443", LeaseSeconds: v}
		got, err := Parse(ann, svcPorts(tcp(443)), defLease)
		if err != nil || got.LeaseSeconds != want {
			t.Errorf("lease=%q: got %d, %v; want %d", v, got.LeaseSeconds, err, want)
		}
	}
	for _, v := range []string{"-1", "abc", "4294967296", "1.5"} {
		ann := map[string]string{TCPEnabled: "true", TCPPorts: "443", LeaseSeconds: v}
		if _, err := Parse(ann, svcPorts(tcp(443)), defLease); err == nil {
			t.Errorf("lease=%q: want error", v)
		}
	}
}

func TestIsManaged_TCPOrUDP(t *testing.T) { // D8, FR-SVC-1
	cases := []struct {
		ann  map[string]string
		want bool
	}{
		{nil, false},
		{map[string]string{TCPEnabled: "true"}, true},
		{map[string]string{UDPEnabled: "true"}, true},
		{map[string]string{TCPEnabled: "false", UDPEnabled: "true"}, true},
		{map[string]string{TCPEnabled: "false", UDPEnabled: "false"}, false},
		{map[string]string{"other": "true"}, false},
	}
	for _, c := range cases {
		if got := IsManaged(c.ann); got != c.want {
			t.Errorf("IsManaged(%v)=%v, want %v", c.ann, got, c.want)
		}
	}
}

func FuzzParsePorts(f *testing.F) {
	for _, s := range []string{"443", "443,32400", " 1 , 65535 ", "8443:443", "", ",", "a", "1:2:3", "-1", "99999999999999999999"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		pairs, err := ParsePorts(s)
		if err != nil {
			return
		}
		for _, p := range pairs {
			if p.External == 0 || p.Internal == 0 {
				t.Fatalf("ParsePorts(%q) returned zero port %v", s, p)
			}
		}
	})
}
