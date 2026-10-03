package upnp

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"net/url"
	"sync"
	"time"

	"github.com/huin/goupnp"
	"github.com/huin/goupnp/dcps/internetgateway2"
	"golang.org/x/time/rate"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/clock"
)

// DeviceStatus describes the router and its WAN connection.
type DeviceStatus struct {
	FriendlyName string
	UDN          string
	Location     string
	ServiceType  string
	// InternalIP is the router's LAN address (the description URL's host).
	InternalIP       string
	ExternalIP       string
	ConnectionStatus string
	Uptime           uint32 // seconds
}

// TrafficStats are the router's WAN byte counters.
type TrafficStats struct {
	BytesSent     uint64
	BytesReceived uint64
}

// Client is what the controllers need from a router.
type Client interface {
	Status(ctx context.Context) (DeviceStatus, error)
	Traffic(ctx context.Context) (TrafficStats, error)
	// List returns every entry in the router's table.
	List(ctx context.Context) ([]PortMapping, error)
	// Get returns ErrNoSuchEntry when the mapping is absent.
	Get(ctx context.Context, proto corev1.Protocol, ext uint16) (PortMapping, error)
	Add(ctx context.Context, m PortMapping) error
	Delete(ctx context.Context, proto corev1.Protocol, ext uint16) error
	// Invalidate drops the cached device; the next call re-discovers.
	Invalidate()
}

// conn is the method set shared by goupnp's WANIPConnection1,
// WANIPConnection2 and WANPPPConnection1 clients.
type conn interface {
	AddPortMappingCtx(ctx context.Context, remoteHost string, extPort uint16, proto string, intPort uint16, intClient string, enabled bool, desc string, lease uint32) error
	DeletePortMappingCtx(ctx context.Context, remoteHost string, extPort uint16, proto string) error
	GetGenericPortMappingEntryCtx(ctx context.Context, index uint16) (remoteHost string, extPort uint16, proto string, intPort uint16, intClient string, enabled bool, desc string, lease uint32, err error)
	GetSpecificPortMappingEntryCtx(ctx context.Context, remoteHost string, extPort uint16, proto string) (intPort uint16, intClient string, enabled bool, desc string, lease uint32, err error)
	GetExternalIPAddressCtx(ctx context.Context) (string, error)
	GetStatusInfoCtx(ctx context.Context) (status string, lastErr string, uptime uint32, err error)
}

type device struct {
	conn        conn
	common      *internetgateway2.WANCommonInterfaceConfig1 // nil if not advertised
	serviceType string
	location    *url.URL
	root        *goupnp.RootDevice
}

// Config configures a GoUPnPClient. Zero values take the defaults noted.
type Config struct {
	// IGDURL, if set, is the root device description URL; SSDP is skipped.
	IGDURL *url.URL
	// Searcher finds devices when IGDURL is unset. Default: SSDPSearcher.
	Searcher Searcher
	// SOAPTimeout bounds each SOAP call. Default 5s.
	SOAPTimeout time.Duration
	// Limiter rate-limits SOAP calls. Default 5/s, burst 10.
	Limiter *rate.Limiter
	// Clock is used for discovery backoff. Default: real clock.
	Clock clock.PassiveClock
	// Backoff between failed discoveries. Default 2s→60s, 20% jitter.
	Backoff *Backoff
}

// GoUPnPClient is a Client backed by goupnp. It discovers the router
// lazily, caches the SOAP clients, and drops them on any transport error so
// the next call re-discovers. It is safe for concurrent use.
type GoUPnPClient struct {
	cfg Config

	mu          sync.Mutex
	dev         *device
	nextAttempt time.Time
}

var _ Client = (*GoUPnPClient)(nil)

// New returns a client; it does no I/O until the first call.
func New(cfg Config) *GoUPnPClient {
	if cfg.Searcher == nil {
		cfg.Searcher = SSDPSearcher{}
	}
	if cfg.SOAPTimeout == 0 {
		cfg.SOAPTimeout = 5 * time.Second
	}
	if cfg.Limiter == nil {
		cfg.Limiter = rate.NewLimiter(5, 10)
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.RealClock{}
	}
	if cfg.Backoff == nil {
		cfg.Backoff = &Backoff{Initial: 2 * time.Second, Max: 60 * time.Second, Jitter: 0.2, Rand: rand.Float64}
	}
	return &GoUPnPClient{cfg: cfg}
}

// Invalidate implements Client.
func (c *GoUPnPClient) Invalidate() {
	c.mu.Lock()
	c.dev = nil
	c.mu.Unlock()
}

func (c *GoUPnPClient) device(ctx context.Context) (*device, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dev != nil {
		return c.dev, nil
	}
	if now := c.cfg.Clock.Now(); now.Before(c.nextAttempt) {
		return nil, fmt.Errorf("%w: discovery backing off for %s", ErrUnreachable, c.nextAttempt.Sub(now))
	}
	dev, err := c.discover(ctx)
	if err != nil {
		c.nextAttempt = c.cfg.Clock.Now().Add(c.cfg.Backoff.Next())
		return nil, err
	}
	c.cfg.Backoff.Reset()
	c.dev = dev
	return dev, nil
}

// do runs one SOAP call against the current device.
func (c *GoUPnPClient) do(ctx context.Context, fn func(context.Context, *device) error) error {
	dev, err := c.device(ctx)
	if err != nil {
		return err
	}
	if err := c.cfg.Limiter.Wait(ctx); err != nil {
		return fmt.Errorf("upnp rate limit: %w", err)
	}
	cctx, cancel := context.WithTimeout(ctx, c.cfg.SOAPTimeout)
	defer cancel()
	err = Classify(fn(cctx, dev))
	if errors.Is(err, ErrUnreachable) {
		c.mu.Lock()
		if c.dev == dev {
			c.dev = nil
		}
		c.mu.Unlock()
	}
	return err
}

// Status implements Client.
func (c *GoUPnPClient) Status(ctx context.Context) (DeviceStatus, error) {
	var st DeviceStatus
	err := c.do(ctx, func(ctx context.Context, d *device) error {
		st = DeviceStatus{
			FriendlyName: d.root.Device.FriendlyName,
			UDN:          d.root.Device.UDN,
			Location:     d.location.String(),
			ServiceType:  d.serviceType,
			InternalIP:   d.location.Hostname(),
		}
		var err error
		st.ConnectionStatus, _, st.Uptime, err = d.conn.GetStatusInfoCtx(ctx)
		return err
	})
	if err != nil {
		return DeviceStatus{}, err
	}
	err = c.do(ctx, func(ctx context.Context, d *device) error {
		var err error
		st.ExternalIP, err = d.conn.GetExternalIPAddressCtx(ctx)
		return err
	})
	if err != nil {
		return DeviceStatus{}, err
	}
	return st, nil
}

// ErrNoTrafficCounters is returned by Traffic when the router has no
// WANCommonInterfaceConfig service.
var ErrNoTrafficCounters = errors.New("router does not expose WANCommonInterfaceConfig")

// Traffic implements Client.
func (c *GoUPnPClient) Traffic(ctx context.Context) (TrafficStats, error) {
	dev, err := c.device(ctx)
	if err != nil {
		return TrafficStats{}, err
	}
	if dev.common == nil {
		return TrafficStats{}, ErrNoTrafficCounters
	}
	var tr TrafficStats
	err = c.do(ctx, func(ctx context.Context, d *device) error {
		var err error
		tr.BytesSent, err = d.common.GetTotalBytesSentCtx(ctx)
		return err
	})
	if err != nil {
		return TrafficStats{}, err
	}
	err = c.do(ctx, func(ctx context.Context, d *device) error {
		var err error
		tr.BytesReceived, err = d.common.GetTotalBytesReceivedCtx(ctx)
		return err
	})
	if err != nil {
		return TrafficStats{}, err
	}
	return tr, nil
}

func toProtocol(s string) (corev1.Protocol, bool) {
	switch p := corev1.Protocol(s); p {
	case corev1.ProtocolTCP, corev1.ProtocolUDP:
		return p, true
	}
	return "", false
}

// List implements Client. Entries with protocols other than TCP/UDP are skipped.
func (c *GoUPnPClient) List(ctx context.Context) ([]PortMapping, error) {
	var out []PortMapping
	for i := uint16(0); ; i++ {
		var m PortMapping
		var proto string
		err := c.do(ctx, func(ctx context.Context, d *device) error {
			var err error
			_, m.ExternalPort, proto, m.InternalPort, m.InternalClient, m.Enabled, m.Description, m.LeaseDuration, err =
				d.conn.GetGenericPortMappingEntryCtx(ctx, i)
			return err
		})
		if errors.Is(err, ErrEndOfList) || errors.Is(err, ErrNoSuchEntry) {
			break
		}
		if err != nil {
			return nil, err
		}
		var ok bool
		if m.Protocol, ok = toProtocol(proto); ok {
			out = append(out, m)
		}
		if i == math.MaxUint16 {
			break
		}
	}
	return out, nil
}

// Get implements Client.
func (c *GoUPnPClient) Get(ctx context.Context, proto corev1.Protocol, ext uint16) (PortMapping, error) {
	m := PortMapping{Protocol: proto, ExternalPort: ext}
	err := c.do(ctx, func(ctx context.Context, d *device) error {
		var err error
		m.InternalPort, m.InternalClient, m.Enabled, m.Description, m.LeaseDuration, err =
			d.conn.GetSpecificPortMappingEntryCtx(ctx, "", ext, string(proto))
		return err
	})
	if err != nil {
		return PortMapping{}, err
	}
	return m, nil
}

// Add implements Client. If the router only supports permanent leases
// (725), the add is retried once with lease 0.
func (c *GoUPnPClient) Add(ctx context.Context, m PortMapping) error {
	add := func(lease uint32) error {
		return c.do(ctx, func(ctx context.Context, d *device) error {
			return d.conn.AddPortMappingCtx(ctx, "", m.ExternalPort, string(m.Protocol), m.InternalPort, m.InternalClient, m.Enabled, m.Description, lease)
		})
	}
	err := add(m.LeaseDuration)
	if errors.Is(err, ErrOnlyPermanentLeases) && m.LeaseDuration != 0 {
		err = add(0)
	}
	return err
}

// Delete implements Client.
func (c *GoUPnPClient) Delete(ctx context.Context, proto corev1.Protocol, ext uint16) error {
	return c.do(ctx, func(ctx context.Context, d *device) error {
		return d.conn.DeletePortMappingCtx(ctx, "", ext, string(proto))
	})
}
