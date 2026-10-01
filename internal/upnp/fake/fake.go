// Package fake is an in-memory upnp.Client for unit tests.
package fake

import (
	"context"
	"slices"
	"sync"

	corev1 "k8s.io/api/core/v1"

	"github.com/nashant/upnp-nat-controller/internal/upnp"
)

type key struct {
	proto corev1.Protocol
	port  uint16
}

// Client is an in-memory router. Add of an existing key with a different
// internal client fails with ErrConflict; Get/Delete of a missing key fail
// with ErrNoSuchEntry. Errors set with SetError are returned instead.
type Client struct {
	mu      sync.Mutex
	status  upnp.DeviceStatus
	traffic upnp.TrafficStats
	table   map[key]upnp.PortMapping
	errs    map[string]error
	calls   map[string]int
}

var _ upnp.Client = (*Client)(nil)

// New returns an empty, healthy fake router.
func New() *Client {
	return &Client{
		status: upnp.DeviceStatus{
			FriendlyName: "Fake IGD", Location: "http://192.168.1.1:5000/rootDesc.xml", InternalIP: "192.168.1.1",
			ServiceType: "urn:schemas-upnp-org:service:WANIPConnection:2", ExternalIP: "81.2.69.142", ConnectionStatus: "Connected", Uptime: 1000,
		},
		table: map[key]upnp.PortMapping{},
		errs:  map[string]error{},
		calls: map[string]int{},
	}
}

// SetError makes method (e.g. "List") return err; nil clears it.
func (c *Client) SetError(method string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil {
		delete(c.errs, method)
		return
	}
	c.errs[method] = err
}

// SetStatus sets what Status returns.
func (c *Client) SetStatus(st upnp.DeviceStatus) { c.mu.Lock(); c.status = st; c.mu.Unlock() }

// UpdateStatus modifies what Status returns.
func (c *Client) UpdateStatus(fn func(*upnp.DeviceStatus)) { c.mu.Lock(); fn(&c.status); c.mu.Unlock() }

// SetTraffic sets what Traffic returns.
func (c *Client) SetTraffic(tr upnp.TrafficStats) { c.mu.Lock(); c.traffic = tr; c.mu.Unlock() }

// Seed inserts mappings directly.
func (c *Client) Seed(ms ...upnp.PortMapping) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range ms {
		c.table[key{m.Protocol, m.ExternalPort}] = m
	}
}

// Clear empties the table, like a router reboot.
func (c *Client) Clear() { c.mu.Lock(); c.table = map[key]upnp.PortMapping{}; c.mu.Unlock() }

// Mappings returns the table sorted by protocol then port.
func (c *Client) Mappings() []upnp.PortMapping {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]upnp.PortMapping, 0, len(c.table))
	for _, m := range c.table {
		out = append(out, m)
	}
	slices.SortFunc(out, func(a, b upnp.PortMapping) int {
		return upnp.ComparePortKey(a.Protocol, a.ExternalPort, b.Protocol, b.ExternalPort)
	})
	return out
}

// Count returns how many times method was called.
func (c *Client) Count(method string) int { c.mu.Lock(); defer c.mu.Unlock(); return c.calls[method] }

// ResetCalls zeroes the call counters.
func (c *Client) ResetCalls() { c.mu.Lock(); c.calls = map[string]int{}; c.mu.Unlock() }

// call records a call and returns its injected error. c.mu must be held.
func (c *Client) call(method string) error {
	c.calls[method]++
	return c.errs[method]
}

// Status implements upnp.Client.
func (c *Client) Status(context.Context) (upnp.DeviceStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.call("Status"); err != nil {
		return upnp.DeviceStatus{}, err
	}
	return c.status, nil
}

// Traffic implements upnp.Client.
func (c *Client) Traffic(context.Context) (upnp.TrafficStats, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.call("Traffic"); err != nil {
		return upnp.TrafficStats{}, err
	}
	return c.traffic, nil
}

// List implements upnp.Client.
func (c *Client) List(context.Context) ([]upnp.PortMapping, error) {
	c.mu.Lock()
	err := c.call("List")
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return c.Mappings(), nil
}

// Get implements upnp.Client.
func (c *Client) Get(_ context.Context, proto corev1.Protocol, ext uint16) (upnp.PortMapping, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.call("Get"); err != nil {
		return upnp.PortMapping{}, err
	}
	m, ok := c.table[key{proto, ext}]
	if !ok {
		return upnp.PortMapping{}, upnp.ErrNoSuchEntry
	}
	return m, nil
}

// Add implements upnp.Client.
func (c *Client) Add(_ context.Context, m upnp.PortMapping) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.call("Add"); err != nil {
		return err
	}
	k := key{m.Protocol, m.ExternalPort}
	if old, ok := c.table[k]; ok && old.InternalClient != m.InternalClient {
		return upnp.ErrConflict
	}
	c.table[k] = m
	return nil
}

// Delete implements upnp.Client.
func (c *Client) Delete(_ context.Context, proto corev1.Protocol, ext uint16) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.call("Delete"); err != nil {
		return err
	}
	k := key{proto, ext}
	if _, ok := c.table[k]; !ok {
		return upnp.ErrNoSuchEntry
	}
	delete(c.table, k)
	return nil
}

// Invalidate implements upnp.Client.
func (c *Client) Invalidate() { c.mu.Lock(); c.calls["Invalidate"]++; c.mu.Unlock() }
