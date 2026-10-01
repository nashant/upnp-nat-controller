package upnp_test

import (
	"context"
	"errors"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"golang.org/x/time/rate"
	corev1 "k8s.io/api/core/v1"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/nashant/upnp-nat-controller/internal/upnp"
	"github.com/nashant/upnp-nat-controller/internal/upnp/fakeigd"
)

var ctx = context.Background()

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// searcherFor follows the fake's current URL, as SSDP would after a restart.
func searcherFor(f *fakeigd.Server, calls *atomic.Int32) upnp.Searcher {
	return upnp.SearcherFunc(func(context.Context) ([]*url.URL, error) {
		if calls != nil {
			calls.Add(1)
		}
		u, _ := url.Parse(f.URL())
		return []*url.URL{u}, nil
	})
}

func newClient(t *testing.T, cfg upnp.Config) *upnp.GoUPnPClient {
	t.Helper()
	if cfg.SOAPTimeout == 0 {
		cfg.SOAPTimeout = 2 * time.Second
	}
	return upnp.New(cfg)
}

var m443 = upnp.PortMapping{
	Protocol: corev1.ProtocolTCP, ExternalPort: 443, InternalPort: 443,
	InternalClient: "172.16.1.2", Enabled: true, Description: "upnp-nat-controller/traefik/public-traefik", LeaseDuration: 3600,
}

func TestClient_ByURL_PrefersWANIP2ThenIP1ThenPPP(t *testing.T) { // FR-DISC-1, FR-DISC-2
	cases := []struct {
		svcs []fakeigd.Service
		want string
	}{
		{[]fakeigd.Service{fakeigd.WANPPPConnection1, fakeigd.WANIPConnection1, fakeigd.WANIPConnection2}, "urn:schemas-upnp-org:service:WANIPConnection:2"},
		{[]fakeigd.Service{fakeigd.WANPPPConnection1, fakeigd.WANIPConnection1}, "urn:schemas-upnp-org:service:WANIPConnection:1"},
		{[]fakeigd.Service{fakeigd.WANPPPConnection1}, "urn:schemas-upnp-org:service:WANPPPConnection:1"},
	}
	for _, c := range cases {
		f := fakeigd.New(t, fakeigd.WithServices(c.svcs...), fakeigd.WithDeviceVersion(1))
		cl := newClient(t, upnp.Config{IGDURL: mustURL(t, f.URL())})
		st, err := cl.Status(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if st.ServiceType != c.want {
			t.Errorf("services %v: picked %s, want %s", c.svcs, st.ServiceType, c.want)
		}
	}
}

func TestClient_Search_SkipsDevicesWithoutConnectionService(t *testing.T) { // FR-DISC-2
	bad := fakeigd.New(t, fakeigd.WithServices())
	good := fakeigd.New(t)
	cl := newClient(t, upnp.Config{Searcher: upnp.SearcherFunc(func(context.Context) ([]*url.URL, error) {
		return []*url.URL{mustURL(t, bad.URL()), mustURL(t, good.URL())}, nil
	})})
	st, err := cl.Status(ctx)
	if err != nil || st.Location != good.URL() {
		t.Fatalf("got %+v, %v; want location %s", st, err, good.URL())
	}
}

func TestClient_Status(t *testing.T) {
	clk := clocktesting.NewFakeClock(time.Unix(1_000_000, 0))
	f := fakeigd.New(t, fakeigd.WithClock(clk))
	clk.Step(42 * time.Second)
	cl := newClient(t, upnp.Config{IGDURL: mustURL(t, f.URL())})
	st, err := cl.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := upnp.DeviceStatus{
		FriendlyName: "Fake IGD", UDN: "uuid:00000000-0000-0000-0000-000000000001", Location: f.URL(),
		ServiceType: "urn:schemas-upnp-org:service:WANIPConnection:2", InternalIP: "127.0.0.1",
		ExternalIP: "81.2.69.142", ConnectionStatus: "Connected", Uptime: 42,
	}
	if d := cmp.Diff(want, st); d != "" {
		t.Fatalf("(-want +got)\n%s", d)
	}
}

func TestClient_Traffic(t *testing.T) {
	f := fakeigd.New(t)
	f.SetTraffic(10, 20)
	cl := newClient(t, upnp.Config{IGDURL: mustURL(t, f.URL())})
	tr, err := cl.Traffic(ctx)
	if err != nil || tr != (upnp.TrafficStats{BytesSent: 10, BytesReceived: 20}) {
		t.Fatalf("got %+v %v", tr, err)
	}
}

func TestClient_ListAddGetDelete_RoundTrip(t *testing.T) {
	f := fakeigd.New(t)
	f.AddMapping(fakeigd.Mapping{Protocol: "UDP", ExternalPort: 53, InternalPort: 53, InternalClient: "192.168.1.5", Enabled: true, Description: "dns", Lease: 0})
	cl := newClient(t, upnp.Config{IGDURL: mustURL(t, f.URL())})

	if err := cl.Add(ctx, m443); err != nil {
		t.Fatal(err)
	}
	got, err := cl.Get(ctx, corev1.ProtocolTCP, 443)
	if err != nil {
		t.Fatal(err)
	}
	if d := cmp.Diff(m443, got); d != "" {
		t.Fatalf("Get (-want +got)\n%s", d)
	}

	list, err := cl.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []upnp.PortMapping{
		m443,
		{Protocol: corev1.ProtocolUDP, ExternalPort: 53, InternalPort: 53, InternalClient: "192.168.1.5", Enabled: true, Description: "dns", LeaseDuration: fakeigd.MaxLease},
	}
	if d := cmp.Diff(want, list); d != "" {
		t.Fatalf("List (-want +got)\n%s", d)
	}

	if err := cl.Delete(ctx, corev1.ProtocolTCP, 443); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.Get(ctx, corev1.ProtocolTCP, 443); !errors.Is(err, upnp.ErrNoSuchEntry) {
		t.Fatalf("after delete: %v, want ErrNoSuchEntry", err)
	}
	if err := cl.Delete(ctx, corev1.ProtocolTCP, 443); !errors.Is(err, upnp.ErrNoSuchEntry) {
		t.Fatalf("second delete: %v, want ErrNoSuchEntry", err)
	}
}

func TestClient_List_Empty(t *testing.T) {
	cl := newClient(t, upnp.Config{IGDURL: mustURL(t, fakeigd.New(t).URL())})
	list, err := cl.List(ctx)
	if err != nil || len(list) != 0 {
		t.Fatalf("got %v %v", list, err)
	}
}

func TestClient_Add_ConflictTyped(t *testing.T) {
	f := fakeigd.New(t)
	f.AddMapping(fakeigd.Mapping{Protocol: "TCP", ExternalPort: 443, InternalPort: 443, InternalClient: "192.168.1.50", Enabled: true, Description: "Xbox"})
	cl := newClient(t, upnp.Config{IGDURL: mustURL(t, f.URL())})
	if err := cl.Add(ctx, m443); !errors.Is(err, upnp.ErrConflict) {
		t.Fatalf("got %v, want ErrConflict", err)
	}
	// A SOAP fault is a working router: no rediscovery.
	f.ResetRequests()
	if _, err := cl.List(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestClient_TransportError_Invalidates_NextCallRediscovers(t *testing.T) { // FR-DISC-4, D1–D3
	f := fakeigd.New(t)
	var searches atomic.Int32
	cl := newClient(t, upnp.Config{Searcher: searcherFor(f, &searches)})
	if _, err := cl.List(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.List(ctx); err != nil {
		t.Fatal(err)
	}
	if n := searches.Load(); n != 1 {
		t.Fatalf("searches before failure = %d, want 1 (clients cached)", n)
	}
	f.Stop()
	if _, err := cl.List(ctx); !errors.Is(err, upnp.ErrUnreachable) {
		t.Fatalf("got %v, want ErrUnreachable", err)
	}
	f.Start()
	if _, err := cl.List(ctx); err != nil {
		t.Fatal(err)
	}
	if n := searches.Load(); n != 2 {
		t.Fatalf("searches = %d, want 2 (rediscovered once)", n)
	}
}

func TestClient_Invalidate_ForcesRediscovery(t *testing.T) { // FR-DISC-4
	f := fakeigd.New(t)
	var searches atomic.Int32
	cl := newClient(t, upnp.Config{Searcher: searcherFor(f, &searches)})
	_, _ = cl.List(ctx)
	cl.Invalidate()
	_, _ = cl.List(ctx)
	if n := searches.Load(); n != 2 {
		t.Fatalf("searches = %d, want 2", n)
	}
}

func TestClient_RestartOnNewPort_RecoversWithoutRecreate(t *testing.T) { // D1
	f := fakeigd.New(t)
	cl := newClient(t, upnp.Config{Searcher: searcherFor(f, nil)})
	if err := cl.Add(ctx, m443); err != nil {
		t.Fatal(err)
	}
	f.RestartOnNewPort()
	if _, err := cl.List(ctx); !errors.Is(err, upnp.ErrUnreachable) {
		t.Fatalf("first call after restart: %v, want ErrUnreachable", err)
	}
	if err := cl.Add(ctx, m443); err != nil {
		t.Fatalf("same client after rediscovery: %v", err)
	}
	st, err := cl.Status(ctx)
	if err != nil || st.Location != f.URL() {
		t.Fatalf("status %+v %v, want location %s", st, err, f.URL())
	}
	if len(f.Mappings()) != 1 {
		t.Fatalf("mappings %v", f.Mappings())
	}
}

func TestClient_PerCallTimeout(t *testing.T) { // FR-DISC-6
	f := fakeigd.New(t)
	cl := upnp.New(upnp.Config{IGDURL: mustURL(t, f.URL()), SOAPTimeout: 100 * time.Millisecond})
	if _, err := cl.Status(ctx); err != nil {
		t.Fatal(err)
	}
	f.SetHang(true)
	start := time.Now()
	_, err := cl.List(ctx)
	if !errors.Is(err, upnp.ErrUnreachable) {
		t.Fatalf("got %v, want ErrUnreachable", err)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("call took %v, timeout not applied", el)
	}
}

func TestClient_RateLimited(t *testing.T) { // FR-DISC-6
	f := fakeigd.New(t)
	cl := newClient(t, upnp.Config{IGDURL: mustURL(t, f.URL()), Limiter: rate.NewLimiter(rate.Every(time.Hour), 3)})
	// Discovery is not rate limited; Status makes two SOAP calls, Get one.
	if _, err := cl.Status(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.Get(ctx, corev1.ProtocolTCP, 1); !errors.Is(err, upnp.ErrNoSuchEntry) {
		t.Fatal(err)
	}
	tctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	f.ResetRequests()
	_, err := cl.Get(tctx, corev1.ProtocolTCP, 1)
	if err == nil || errors.Is(err, upnp.ErrNoSuchEntry) {
		t.Fatalf("4th call not limited: %v", err)
	}
	if errors.Is(err, upnp.ErrUnreachable) {
		t.Fatalf("rate limiting is not a router failure: %v", err)
	}
	if n := len(f.Requests()); n != 0 {
		t.Fatalf("router saw %d requests while limited", n)
	}
}

func TestClient_OnlyPermanentLeases_RetriesWithZero(t *testing.T) { // FR-DISC-7, 725
	f := fakeigd.New(t, fakeigd.OnlyPermanentLeases())
	cl := newClient(t, upnp.Config{IGDURL: mustURL(t, f.URL())})
	if err := cl.Add(ctx, m443); err != nil {
		t.Fatal(err)
	}
	if n := f.Count("AddPortMapping"); n != 2 {
		t.Fatalf("AddPortMapping calls %d, want 2", n)
	}
	if ms := f.Mappings(); len(ms) != 1 || ms[0].Lease != fakeigd.MaxLease {
		t.Fatalf("got %+v", ms)
	}
}

func TestClient_RejectsControlURLOnDifferentHost(t *testing.T) { // NFR-SEC-2
	f := fakeigd.New(t, fakeigd.WithControlHost("10.99.99.99:5000"))
	cl := newClient(t, upnp.Config{IGDURL: mustURL(t, f.URL())})
	_, err := cl.Status(ctx)
	if !errors.Is(err, upnp.ErrUntrustedDevice) {
		t.Fatalf("got %v, want ErrUntrustedDevice", err)
	}
}

func TestDiscovery_NoDevice_ReturnsErrorNotPanic(t *testing.T) { // D9, FR-DISC-3
	cl := newClient(t, upnp.Config{Searcher: upnp.SearcherFunc(func(context.Context) ([]*url.URL, error) { return nil, nil })})
	if _, err := cl.Status(ctx); !errors.Is(err, upnp.ErrNoDevice) || !errors.Is(err, upnp.ErrUnreachable) {
		t.Fatalf("got %v", err)
	}
	if _, err := cl.List(ctx); err == nil {
		t.Fatal("want error")
	}
}

func TestDiscovery_SearchError_Unreachable(t *testing.T) { // FR-DISC-3
	cl := newClient(t, upnp.Config{Searcher: upnp.SearcherFunc(func(context.Context) ([]*url.URL, error) {
		return nil, errors.New("no multicast")
	})})
	if _, err := cl.Status(ctx); !errors.Is(err, upnp.ErrUnreachable) {
		t.Fatalf("got %v", err)
	}
}

func TestDiscovery_BackoffSchedule_2sTo60s(t *testing.T) { // FR-DISC-3
	b := upnp.Backoff{Initial: 2 * time.Second, Max: 60 * time.Second}
	var got []time.Duration
	for i := 0; i < 8; i++ {
		got = append(got, b.Next())
	}
	want := []time.Duration{2, 4, 8, 16, 32, 60, 60, 60}
	for i := range want {
		want[i] *= time.Second
	}
	if d := cmp.Diff(want, got); d != "" {
		t.Fatalf("(-want +got)\n%s", d)
	}
	b.Reset()
	if d := b.Next(); d != 2*time.Second {
		t.Fatalf("after reset %v", d)
	}
}

func TestDiscovery_BackoffJitterBounded(t *testing.T) { // FR-DISC-3
	for _, r := range []float64{0, 0.5, 0.999} {
		b := upnp.Backoff{Initial: 2 * time.Second, Max: 60 * time.Second, Jitter: 0.2, Rand: func() float64 { return r }}
		if d := b.Next(); d < 2*time.Second || d > 2400*time.Millisecond {
			t.Errorf("rand=%v first=%v, want [2s,2.4s]", r, d)
		}
		for i := 0; i < 10; i++ {
			b.Next()
		}
		if d := b.Next(); d > 60*time.Second {
			t.Errorf("rand=%v capped=%v > 60s", r, d)
		}
	}
}

func TestDiscovery_ClientBacksOffBetweenAttempts(t *testing.T) { // FR-DISC-3
	clk := clocktesting.NewFakeClock(time.Unix(1_000_000, 0))
	var searches atomic.Int32
	var f *fakeigd.Server
	cl := newClient(t, upnp.Config{
		Clock:   clk,
		Backoff: &upnp.Backoff{Initial: 2 * time.Second, Max: 60 * time.Second},
		Searcher: upnp.SearcherFunc(func(context.Context) ([]*url.URL, error) {
			searches.Add(1)
			if f == nil {
				return nil, nil
			}
			return []*url.URL{mustURL(t, f.URL())}, nil
		}),
	})
	call := func() error { _, err := cl.Status(ctx); return err }

	_ = call() // attempt 1 fails → next allowed in 2s
	_ = call() // backing off, no search
	if n := searches.Load(); n != 1 {
		t.Fatalf("searches %d, want 1", n)
	}
	clk.Step(2 * time.Second)
	_ = call() // attempt 2 fails → next in 4s
	clk.Step(2 * time.Second)
	_ = call()
	if n := searches.Load(); n != 2 {
		t.Fatalf("searches %d, want 2", n)
	}
	f = fakeigd.New(t) // S3: router comes up
	clk.Step(2 * time.Second)
	if err := call(); err != nil {
		t.Fatalf("after router up: %v", err)
	}
	if n := searches.Load(); n != 3 {
		t.Fatalf("searches %d, want 3", n)
	}
}

func TestClient_Traffic_NoCommonInterfaceConfig(t *testing.T) {
	f := fakeigd.New(t, fakeigd.WithoutTrafficCounters())
	var searches atomic.Int32
	cl := newClient(t, upnp.Config{Searcher: searcherFor(f, &searches)})
	if _, err := cl.Traffic(ctx); !errors.Is(err, upnp.ErrNoTrafficCounters) {
		t.Fatalf("got %v", err)
	}
	if _, err := cl.Status(ctx); err != nil || searches.Load() != 1 {
		t.Fatalf("missing counters must not invalidate: %v, searches %d", err, searches.Load())
	}
}

func TestClient_Traffic_Unreachable(t *testing.T) {
	f := fakeigd.New(t, fakeigd.Stopped())
	cl := newClient(t, upnp.Config{IGDURL: mustURL(t, f.URL())})
	if _, err := cl.Traffic(ctx); !errors.Is(err, upnp.ErrUnreachable) {
		t.Fatalf("got %v", err)
	}
}

func TestClient_List_SkipsNonTCPUDP(t *testing.T) {
	f := fakeigd.New(t)
	f.AddMapping(fakeigd.Mapping{Protocol: "GRE", ExternalPort: 1, InternalPort: 1, InternalClient: "192.168.1.5"})
	f.AddMapping(fakeigd.Mapping{Protocol: "TCP", ExternalPort: 2, InternalPort: 2, InternalClient: "192.168.1.5"})
	cl := newClient(t, upnp.Config{IGDURL: mustURL(t, f.URL())})
	list, err := cl.List(ctx)
	if err != nil || len(list) != 1 || list[0].ExternalPort != 2 {
		t.Fatalf("got %+v %v", list, err)
	}
}

func TestFaultError_Message(t *testing.T) {
	f := fakeigd.New(t)
	f.SetFault("GetSpecificPortMappingEntry", 606)
	cl := newClient(t, upnp.Config{IGDURL: mustURL(t, f.URL())})
	_, err := cl.Get(ctx, corev1.ProtocolTCP, 1)
	if !errors.Is(err, upnp.ErrNotAuthorized) || err.Error() != "UPnP error 606: Forced" {
		t.Fatalf("got %v", err)
	}
}
