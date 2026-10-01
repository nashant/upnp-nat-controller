// Package fakeigd is an in-process UPnP Internet Gateway Device that speaks
// real HTTP and SOAP, with miniupnpd-like semantics and fault injection.
// It is for tests only.
package fakeigd

import (
	"bytes"
	"cmp"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/utils/clock"
)

// Service is a WAN connection service type the fake can advertise.
type Service string

// Connection services.
const (
	WANIPConnection1  Service = "WANIPConnection:1"
	WANIPConnection2  Service = "WANIPConnection:2"
	WANPPPConnection1 Service = "WANPPPConnection:1"
)

const (
	serviceURNPrefix = "urn:schemas-upnp-org:service:"
	commonIfConfig   = "WANCommonInterfaceConfig:1"
	descPath         = "/rootDesc.xml"
	// MaxLease is what lease 0 ("infinite") is stored as, like miniupnpd.
	MaxLease = 604800
)

// Mapping is one entry in the fake's port mapping table.
type Mapping struct {
	Protocol       string
	ExternalPort   uint16
	InternalPort   uint16
	InternalClient string
	Enabled        bool
	Description    string
	// Lease is the remaining lease in seconds when read, or the requested
	// lease when passed to AddMapping (0 is stored as MaxLease).
	Lease uint32
}

// Request is one SOAP request received by the fake.
type Request struct {
	Action  string
	Service string
	Args    map[string]string
}

type key struct {
	proto string
	port  uint16
}

type entry struct {
	m       Mapping
	expires time.Time
}

// Server is a fake IGD. All methods are safe for concurrent use.
type Server struct {
	t testing.TB

	mu            sync.Mutex
	clock         clock.PassiveClock
	deviceVersion int
	services      []Service
	controlHost   string
	onlyPermanent bool
	table         map[key]entry
	started       time.Time
	externalIP    string
	connStatus    string
	sent, recv    uint64
	faults        map[string]int
	hang          chan struct{}
	requests      []Request
	addr          string
	srv           *httptest.Server
}

// Option configures a Server.
type Option func(*Server)

// WithClock sets the clock used for leases and uptime.
func WithClock(c clock.PassiveClock) Option { return func(s *Server) { s.clock = c } }

// WithDeviceVersion sets the InternetGatewayDevice version (1 or 2).
func WithDeviceVersion(v int) Option { return func(s *Server) { s.deviceVersion = v } }

// WithServices sets the advertised connection services, in order.
func WithServices(svcs ...Service) Option { return func(s *Server) { s.services = svcs } }

// WithControlHost advertises control URLs on host instead of the server's own address.
func WithControlHost(host string) Option { return func(s *Server) { s.controlHost = host } }

// OnlyPermanentLeases makes AddPortMapping with a non-zero lease fail with 725.
func OnlyPermanentLeases() Option { return func(s *Server) { s.onlyPermanent = true } }

// Stopped creates the server without starting it; call Start.
func Stopped() Option { return func(s *Server) { s.srv = nil; s.addr = "stopped" } }

// New creates and starts a fake IGD that is stopped when t ends.
func New(t testing.TB, opts ...Option) *Server {
	t.Helper()
	s := &Server{
		t:             t,
		clock:         clock.RealClock{},
		deviceVersion: 2,
		services:      []Service{WANIPConnection2},
		table:         map[key]entry{},
		externalIP:    "81.2.69.142",
		connStatus:    "Connected",
		faults:        map[string]int{},
	}
	for _, o := range opts {
		o(s)
	}
	s.started = s.clock.Now()
	stopped := s.addr == "stopped"
	s.addr = freeAddr(t)
	if !stopped {
		s.Start()
	}
	t.Cleanup(s.Stop)
	return s
}

func freeAddr(t testing.TB) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fakeigd: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// URL is the root device description URL.
func (s *Server) URL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return "http://" + s.addr + descPath
}

// Start starts serving on the server's current address.
func (s *Server) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.srv != nil {
		return
	}
	var (
		l   net.Listener
		err error
	)
	for i := 0; i < 50; i++ { // the port may linger briefly after Stop
		if l, err = net.Listen("tcp", s.addr); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		s.t.Fatalf("fakeigd: listen %s: %v", s.addr, err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(s.serveHTTP))
	_ = srv.Listener.Close()
	srv.Listener = l
	srv.Start()
	s.srv = srv
}

// Stop stops serving and drops open connections.
func (s *Server) Stop() {
	s.mu.Lock()
	srv := s.srv
	s.srv = nil
	if s.hang != nil {
		close(s.hang)
		s.hang = nil
	}
	s.mu.Unlock()
	if srv != nil {
		srv.CloseClientConnections()
		srv.Close()
	}
}

func (s *Server) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.table = map[key]entry{}
	s.started = s.clock.Now()
}

// Restart simulates the UPnP daemon restarting on the same port: the table
// is cleared, uptime resets and open connections are dropped.
func (s *Server) Restart() {
	s.Stop()
	s.reset()
	s.Start()
}

// RestartOnNewPort is Restart, but the daemon comes back on a new port, so
// the old description and control URLs stop working.
func (s *Server) RestartOnNewPort() {
	s.Stop()
	s.reset()
	addr := freeAddr(s.t)
	s.mu.Lock()
	s.addr = addr
	s.mu.Unlock()
	s.Start()
}

// SetExternalIP sets the WAN address returned by GetExternalIPAddress.
func (s *Server) SetExternalIP(ip string) { s.mu.Lock(); s.externalIP = ip; s.mu.Unlock() }

// SetConnectionStatus sets the status returned by GetStatusInfo.
func (s *Server) SetConnectionStatus(st string) { s.mu.Lock(); s.connStatus = st; s.mu.Unlock() }

// SetTraffic sets the WAN byte counters.
func (s *Server) SetTraffic(sent, recv uint64) {
	s.mu.Lock()
	s.sent, s.recv = sent, recv
	s.mu.Unlock()
}

// SetFault makes every call of action fail with the UPnP error code; 0 clears it.
func (s *Server) SetFault(action string, code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if code == 0 {
		delete(s.faults, action)
		return
	}
	s.faults[action] = code
}

// SetHang makes SOAP requests block until the client gives up or SetHang(false).
func (s *Server) SetHang(hang bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case hang && s.hang == nil:
		s.hang = make(chan struct{})
	case !hang && s.hang != nil:
		close(s.hang)
		s.hang = nil
	}
}

// AddMapping inserts a mapping directly, as if another client had added it.
func (s *Server) AddMapping(m Mapping) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.put(m)
}

func (s *Server) put(m Mapping) {
	if m.Lease == 0 {
		m.Lease = MaxLease
	}
	s.table[key{m.Protocol, m.ExternalPort}] = entry{m: m, expires: s.clock.Now().Add(time.Duration(m.Lease) * time.Second)}
}

// Mappings returns the live table, sorted by protocol then port, with
// remaining leases.
func (s *Server) Mappings() []Mapping {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.live()
}

func (s *Server) live() []Mapping {
	now := s.clock.Now()
	out := make([]Mapping, 0, len(s.table))
	for k, e := range s.table {
		if !now.Before(e.expires) {
			delete(s.table, k)
			continue
		}
		m := e.m
		m.Lease = uint32((e.expires.Sub(now) + time.Second - 1) / time.Second)
		out = append(out, m)
	}
	slices.SortFunc(out, func(a, b Mapping) int {
		return cmp.Or(cmp.Compare(a.Protocol, b.Protocol), cmp.Compare(a.ExternalPort, b.ExternalPort))
	})
	return out
}

// Requests returns the SOAP requests received so far.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// Count returns how many requests for action were received.
func (s *Server) Count(action string) int {
	n := 0
	for _, r := range s.Requests() {
		if r.Action == action {
			n++
		}
	}
	return n
}

// ResetRequests clears the request log.
func (s *Server) ResetRequests() { s.mu.Lock(); s.requests = nil; s.mu.Unlock() }

func ctlPath(svcType string) string {
	return "/ctl/" + strings.ReplaceAll(svcType, ":", "")
}

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == descPath {
		w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
		_, _ = io.WriteString(w, s.description())
		return
	}
	svcType, ok := s.serviceForPath(r.URL.Path)
	if r.Method != http.MethodPost || !ok {
		http.NotFound(w, r)
		return
	}

	s.mu.Lock()
	hang := s.hang
	s.mu.Unlock()
	if hang != nil {
		select {
		case <-hang:
		case <-r.Context().Done():
			return
		}
	}

	action, args, err := parseRequest(r.Body)
	if err != nil {
		writeFault(w, 402, "Invalid Args")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, Request{Action: action, Service: svcType, Args: args})
	if code, ok := s.faults[action]; ok {
		writeFault(w, code, "Forced")
		return
	}
	out, code := s.handle(action, args)
	if code != 0 {
		writeFault(w, code, faultName(code))
		return
	}
	writeResponse(w, serviceURNPrefix+svcType, action, out)
}

func (s *Server) serviceForPath(p string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p == ctlPath(commonIfConfig) {
		return commonIfConfig, true
	}
	for _, svc := range s.services {
		if p == ctlPath(string(svc)) {
			return string(svc), true
		}
	}
	return "", false
}

type kv struct{ k, v string }

func b2s(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// handle runs one action with s.mu held. It returns response args, or a
// non-zero UPnP error code.
func (s *Server) handle(action string, a map[string]string) ([]kv, int) {
	switch action {
	case "GetExternalIPAddress":
		return []kv{{"NewExternalIPAddress", s.externalIP}}, 0
	case "GetStatusInfo":
		up := uint32(s.clock.Since(s.started) / time.Second)
		return []kv{{"NewConnectionStatus", s.connStatus}, {"NewLastConnectionError", "ERROR_NONE"}, {"NewUptime", strconv.FormatUint(uint64(up), 10)}}, 0
	case "GetTotalBytesSent":
		return []kv{{"NewTotalBytesSent", strconv.FormatUint(s.sent, 10)}}, 0
	case "GetTotalBytesReceived":
		return []kv{{"NewTotalBytesReceived", strconv.FormatUint(s.recv, 10)}}, 0
	case "AddPortMapping":
		ext, err1 := strconv.ParseUint(a["NewExternalPort"], 10, 16)
		in, err2 := strconv.ParseUint(a["NewInternalPort"], 10, 16)
		lease, err3 := strconv.ParseUint(a["NewLeaseDuration"], 10, 32)
		proto := a["NewProtocol"]
		if err1 != nil || err2 != nil || err3 != nil || ext == 0 || in == 0 || (proto != "TCP" && proto != "UDP") || a["NewInternalClient"] == "" {
			return nil, 402
		}
		if s.onlyPermanent && lease != 0 {
			return nil, 725
		}
		s.live() // purge expired
		if e, ok := s.table[key{proto, uint16(ext)}]; ok && e.m.InternalClient != a["NewInternalClient"] {
			return nil, 718
		}
		s.put(Mapping{
			Protocol: proto, ExternalPort: uint16(ext), InternalPort: uint16(in),
			InternalClient: a["NewInternalClient"], Enabled: a["NewEnabled"] == "1",
			Description: a["NewPortMappingDescription"], Lease: uint32(lease),
		})
		return nil, 0
	case "DeletePortMapping":
		k, ok := parseKey(a)
		if !ok {
			return nil, 402
		}
		s.live()
		if _, found := s.table[k]; !found {
			return nil, 714
		}
		delete(s.table, k)
		return nil, 0
	case "GetSpecificPortMappingEntry":
		k, ok := parseKey(a)
		if !ok {
			return nil, 402
		}
		for _, m := range s.live() {
			if m.Protocol == k.proto && m.ExternalPort == k.port {
				return []kv{
					{"NewInternalPort", strconv.Itoa(int(m.InternalPort))},
					{"NewInternalClient", m.InternalClient},
					{"NewEnabled", b2s(m.Enabled)},
					{"NewPortMappingDescription", m.Description},
					{"NewLeaseDuration", strconv.FormatUint(uint64(m.Lease), 10)},
				}, 0
			}
		}
		return nil, 714
	case "GetGenericPortMappingEntry":
		idx, err := strconv.ParseUint(a["NewPortMappingIndex"], 10, 16)
		if err != nil {
			return nil, 402
		}
		ms := s.live()
		if idx >= uint64(len(ms)) {
			return nil, 713
		}
		m := ms[idx]
		return []kv{
			{"NewRemoteHost", ""},
			{"NewExternalPort", strconv.Itoa(int(m.ExternalPort))},
			{"NewProtocol", m.Protocol},
			{"NewInternalPort", strconv.Itoa(int(m.InternalPort))},
			{"NewInternalClient", m.InternalClient},
			{"NewEnabled", b2s(m.Enabled)},
			{"NewPortMappingDescription", m.Description},
			{"NewLeaseDuration", strconv.FormatUint(uint64(m.Lease), 10)},
		}, 0
	}
	return nil, 401
}

func parseKey(a map[string]string) (key, bool) {
	p, err := strconv.ParseUint(a["NewExternalPort"], 10, 16)
	if err != nil {
		return key{}, false
	}
	return key{a["NewProtocol"], uint16(p)}, true
}

func faultName(code int) string {
	return map[int]string{
		401: "Invalid Action", 402: "Invalid Args", 501: "Action Failed", 606: "Action not authorized",
		713: "SpecifiedArrayIndexInvalid", 714: "NoSuchEntryInArray", 718: "ConflictInMappingEntry",
		725: "OnlyPermanentLeasesSupported",
	}[code]
}

func parseRequest(body io.Reader) (string, map[string]string, error) {
	var env struct {
		Body struct {
			Inner []byte `xml:",innerxml"`
		} `xml:"Body"`
	}
	if err := xml.NewDecoder(body).Decode(&env); err != nil {
		return "", nil, err
	}
	var act struct {
		XMLName xml.Name
		Args    []struct {
			XMLName xml.Name
			Value   string `xml:",chardata"`
		} `xml:",any"`
	}
	if err := xml.Unmarshal(env.Body.Inner, &act); err != nil {
		return "", nil, err
	}
	args := make(map[string]string, len(act.Args))
	for _, a := range act.Args {
		args[a.XMLName.Local] = a.Value
	}
	return act.XMLName.Local, args, nil
}

const envelopeFmt = `<?xml version="1.0"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>%s</s:Body></s:Envelope>`

func esc(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func writeResponse(w http.ResponseWriter, ns, action string, out []kv) {
	var b strings.Builder
	fmt.Fprintf(&b, `<u:%sResponse xmlns:u="%s">`, action, esc(ns))
	for _, p := range out {
		fmt.Fprintf(&b, "<%s>%s</%s>", p.k, esc(p.v), p.k)
	}
	fmt.Fprintf(&b, "</u:%sResponse>", action)
	w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
	_, _ = fmt.Fprintf(w, envelopeFmt, b.String())
}

func writeFault(w http.ResponseWriter, code int, desc string) {
	w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = fmt.Fprintf(w, envelopeFmt, fmt.Sprintf(
		`<s:Fault><faultcode>s:Client</faultcode><faultstring>UPnPError</faultstring><detail><UPnPError xmlns="urn:schemas-upnp-org:control-1-0"><errorCode>%d</errorCode><errorDescription>%s</errorDescription></UPnPError></detail></s:Fault>`,
		code, esc(desc)))
}

func (s *Server) description() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	base := ""
	if s.controlHost != "" {
		base = "http://" + s.controlHost
	}
	svc := func(t, id string) string {
		return fmt.Sprintf(`<service><serviceType>%s%s</serviceType><serviceId>urn:upnp-org:serviceId:%s</serviceId><SCPDURL>/scpd/%s.xml</SCPDURL><controlURL>%s%s</controlURL><eventSubURL>/evt/%s</eventSubURL></service>`,
			serviceURNPrefix, t, id, id, base, ctlPath(t), id)
	}
	var conns strings.Builder
	for _, c := range s.services {
		id := strings.ReplaceAll(string(c), ":", "")
		conns.WriteString(svc(string(c), id))
	}
	v := s.deviceVersion
	return fmt.Sprintf(`<?xml version="1.0"?>
<root xmlns="urn:schemas-upnp-org:device-1-0">
<specVersion><major>1</major><minor>1</minor></specVersion>
<device>
<deviceType>urn:schemas-upnp-org:device:InternetGatewayDevice:%[1]d</deviceType>
<friendlyName>Fake IGD</friendlyName><manufacturer>fakeigd</manufacturer><modelName>fakeigd</modelName>
<UDN>uuid:00000000-0000-0000-0000-000000000001</UDN>
<deviceList><device>
<deviceType>urn:schemas-upnp-org:device:WANDevice:%[1]d</deviceType>
<friendlyName>WANDevice</friendlyName><UDN>uuid:00000000-0000-0000-0000-000000000002</UDN>
<serviceList>%[2]s</serviceList>
<deviceList><device>
<deviceType>urn:schemas-upnp-org:device:WANConnectionDevice:%[1]d</deviceType>
<friendlyName>WANConnectionDevice</friendlyName><UDN>uuid:00000000-0000-0000-0000-000000000003</UDN>
<serviceList>%[3]s</serviceList>
</device></deviceList>
</device></deviceList>
</device>
</root>`, v, svc(commonIfConfig, "WANCommonIFC1"), conns.String())
}
