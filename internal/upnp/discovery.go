package upnp

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/huin/goupnp"
	"github.com/huin/goupnp/dcps/internetgateway2"
)

// Discovery errors. Both wrap ErrUnreachable.
var (
	ErrNoDevice        = fmt.Errorf("%w: no internet gateway device found", ErrUnreachable)
	ErrUntrustedDevice = errors.New("device control URL is not on the device's host")
)

// Searcher finds candidate root device description URLs.
type Searcher interface {
	Search(ctx context.Context) ([]*url.URL, error)
}

// SearcherFunc adapts a function to Searcher.
type SearcherFunc func(ctx context.Context) ([]*url.URL, error)

// Search implements Searcher.
func (f SearcherFunc) Search(ctx context.Context) ([]*url.URL, error) { return f(ctx) }

// SSDPSearcher searches the LAN for InternetGatewayDevice:1 and :2.
type SSDPSearcher struct{}

// Search implements Searcher.
func (SSDPSearcher) Search(ctx context.Context) ([]*url.URL, error) {
	var (
		out  []*url.URL
		seen = map[string]bool{}
		errs []error
	)
	for _, st := range []string{"urn:schemas-upnp-org:device:InternetGatewayDevice:1", "urn:schemas-upnp-org:device:InternetGatewayDevice:2"} {
		devs, err := goupnp.DiscoverDevicesCtx(ctx, st)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, d := range devs {
			if d.Location != nil && !seen[d.Location.String()] {
				seen[d.Location.String()] = true
				out = append(out, d.Location)
			}
		}
	}
	if len(out) == 0 {
		return nil, errors.Join(errs...)
	}
	return out, nil
}

// connectionServices in order of preference (FR-DISC-2).
var connectionServices = []struct {
	urn  string
	wrap func(goupnp.ServiceClient) conn
}{
	{internetgateway2.URN_WANIPConnection_2, func(sc goupnp.ServiceClient) conn { return &internetgateway2.WANIPConnection2{ServiceClient: sc} }},
	{internetgateway2.URN_WANIPConnection_1, func(sc goupnp.ServiceClient) conn { return &internetgateway2.WANIPConnection1{ServiceClient: sc} }},
	{internetgateway2.URN_WANPPPConnection_1, func(sc goupnp.ServiceClient) conn { return &internetgateway2.WANPPPConnection1{ServiceClient: sc} }},
}

func (c *GoUPnPClient) discover(ctx context.Context) (*device, error) {
	locs := []*url.URL{c.cfg.IGDURL}
	if c.cfg.IGDURL == nil {
		var err error
		if locs, err = c.cfg.Searcher.Search(ctx); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrNoDevice, err)
		}
	}
	var errs []error
	for _, loc := range locs {
		dev, err := c.probe(ctx, loc)
		if err == nil {
			return dev, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", loc, err))
	}
	if len(errs) == 0 {
		return nil, ErrNoDevice
	}
	return nil, fmt.Errorf("%w: %w", ErrNoDevice, errors.Join(errs...))
}

// probe fetches one device description and picks its connection service.
func (c *GoUPnPClient) probe(ctx context.Context, loc *url.URL) (*device, error) {
	pctx, cancel := context.WithTimeout(ctx, c.cfg.SOAPTimeout)
	defer cancel()
	root, err := goupnp.DeviceByURLCtx(pctx, loc)
	if err != nil {
		return nil, err
	}
	dev := &device{root: root, location: loc}
	for _, cs := range connectionServices {
		scs, err := goupnp.NewServiceClientsFromRootDevice(root, loc, cs.urn)
		if err != nil || len(scs) == 0 {
			continue
		}
		if err := checkHost(loc, scs[0]); err != nil {
			return nil, err
		}
		dev.conn, dev.serviceType = cs.wrap(scs[0]), cs.urn
		break
	}
	if dev.conn == nil {
		return nil, errors.New("no WANIPConnection or WANPPPConnection service")
	}
	if scs, err := goupnp.NewServiceClientsFromRootDevice(root, loc, internetgateway2.URN_WANCommonInterfaceConfig_1); err == nil && len(scs) > 0 {
		if err := checkHost(loc, scs[0]); err != nil {
			return nil, err
		}
		dev.common = &internetgateway2.WANCommonInterfaceConfig1{ServiceClient: scs[0]}
	}
	return dev, nil
}

// checkHost enforces NFR-SEC-2: control URLs must be on the host that
// served the description.
func checkHost(loc *url.URL, sc goupnp.ServiceClient) error {
	if h := sc.SOAPClient.EndpointURL.Hostname(); h != loc.Hostname() {
		return fmt.Errorf("%w: %s advertises control URL on %s", ErrUntrustedDevice, loc.Hostname(), h)
	}
	return nil
}

// Backoff is a capped exponential backoff with optional jitter.
type Backoff struct {
	Initial, Max time.Duration
	// Jitter adds up to Jitter×delay, using Rand (in [0,1)).
	Jitter float64
	Rand   func() float64
	n      int
}

// Next returns the next delay.
func (b *Backoff) Next() time.Duration {
	d := b.Max
	if b.n < 32 {
		if e := b.Initial << b.n; e > 0 && e < b.Max {
			d = e
			b.n++
		}
	}
	if b.Jitter > 0 && b.Rand != nil {
		d += time.Duration(float64(d) * b.Jitter * b.Rand())
	}
	return min(d, b.Max)
}

// Reset restarts the schedule.
func (b *Backoff) Reset() { b.n = 0 }
