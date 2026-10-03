# Plan: Convert UPnP NAT Controller from Python/Kopf to Go/controller-runtime

## Context

The current Python/Kopf Kubernetes controller manages UPnP port mappings on routers for LoadBalancer services. It has several critical bugs and architectural limitations:

- **Router reboots clear port mappings** with no automatic recovery (the stated problem)
- Only tracks one LoadBalancer in memory (`memo["lb"]`), so multiple services overwrite each other
- SSDP discovery is one-shot with no retry — dead controller if router is unavailable at startup
- `get_svc()` does fresh SSDP broadcast on every service event (slow, redundant)
- Missing `urllib` import makes the error recovery path itself crash
- `get_ports()` returns strings instead of ints
- `KOPF_LB_PARAMS["when"]` only checks TCP (Python `or` between functions returns first truthy)
- No health probes, leader election, metrics, or graceful shutdown

The Go rewrite fixes all of these while adopting standard Kubernetes controller patterns.

## Decisions

- **Framework**: controller-runtime (kubebuilder conventions)
- **UPnP library**: huin/goupnp
- **Recovery**: Periodic reconciliation every 30s (query router, diff against desired state, re-create missing)
- **CRD**: Clean break — new API group `gateway.nashes.uk`, redesigned schema
- **Observability**: IGD CRD for kubectl visibility + Prometheus metrics

## Architecture

Two controllers sharing a single `upnp.Client` instance, registered with one `ctrl.Manager`:

1. **IGD Controller** (`manager.Runnable`) — discovers IGD via SSDP, polls status every 30s, updates the `InternetGatewayDevice` CRD status
2. **Service Reconciler** (`reconcile.Reconciler`) — watches LoadBalancer Services, manages port mappings, requeues every 30s for periodic reconciliation

```
ctrl.Manager
├── IGD Controller (Runnable, ticker-based)
│   └── Polls router status → updates IGD CRD
├── Service Reconciler (watches v1.Service)
│   └── Diffs desired vs actual → adds/deletes port mappings
└── Shared: upnp.Client (thread-safe, mutex-protected)
        └── SSDP discovery, SOAP port mapping CRUD
```

## Project Structure

```
upnp-nat-controller/
├── cmd/main.go                                  # Manager setup, flag parsing, wiring
├── api/v1alpha1/
│   ├── groupversion_info.go                     # SchemeBuilder for gateway.nashes.uk
│   ├── internetgatewaydevice_types.go           # CRD Go types
│   └── zz_generated.deepcopy.go                 # Generated
├── internal/
│   ├── controller/
│   │   ├── igd_controller.go                    # IGD discovery + status polling
│   │   └── service_controller.go                # Service reconciler + port mapping
│   ├── upnp/
│   │   ├── client.go                            # Client interface + implementation
│   │   ├── discovery.go                         # SSDP retry with exponential backoff
│   │   └── portmapping.go                       # AddPortMapping/DeletePortMapping/List
│   ├── annotations/
│   │   └── parser.go                            # Annotation parsing + validation
│   └── metrics/
│       └── metrics.go                           # Prometheus metric definitions
├── helm/                                        # Updated Helm chart
├── Dockerfile                                   # Multi-stage Go build → distroless
├── Makefile
└── go.mod
```

## CRD Design (`gateway.nashes.uk/v1alpha1`)

### Spec (minimal — controller auto-discovers IGD)
- `pollingInterval` (int32, default 30, min 10) — how often to poll the router

### Status
- `friendlyName`, `internalIP`, `externalIP`, `connectionStatus`, `uptime` — same as before
- `lastSeen` (*metav1.Time) — **new**: timestamp of last successful poll
- `portMappings` ([]PortMappingStatus) — only controller-owned mappings (filtered by description prefix)
- `conditions` ([]metav1.Condition) — **new**: `Discovered`, `Reachable`, `Ready`

### Removed from CRD status (moved to Prometheus metrics only)
- `totalBytesSent`, `totalBytesReceived`, `totalPacketsSent`, `totalPacketsReceived` — these are counters that change every poll and cause write amplification in etcd

### PortMappingStatus fields
- `protocol`, `externalPort`, `internalPort`, `internalClient`, `enabled`, `description`, `leaseDuration`
- `serviceRef` (namespace + name) — **new**: traces mapping back to K8s Service

### Ownership model (fixes IP-based ownership)
- Description format: `upnp-nat-controller/<namespace>/<name>`
- Ownership determined by description match, not IP comparison
- Survives controller restarts and IP changes

## Service Reconciler Logic

```
Reconcile(service):
  1. If deleted → delete all mappings with description "upnp-nat-controller/<ns>/<name>"
  2. Parse + validate annotations (tcp/udp.advertise.upnp/enabled, /ports)
     - Invalid → Warning event, no requeue
  3. Get LB IP from status.loadBalancer.ingress[0].ip
     - Not assigned yet → requeue after 5s
  4. Build desired mappings list
  5. For each desired mapping:
     - Query router via GetSpecificPortMappingEntry
     - Missing → AddPortMapping
     - Exists, ours, correct IP → no-op
     - Exists, ours, wrong IP → delete + re-add
     - Exists, not ours → Warning event "PortConflict", skip
  6. Find stale mappings (on router with our description but not in desired set) → delete
  7. Return RequeueAfter: 30s ← THIS IS THE PERIODIC RECONCILIATION THAT FIXES ROUTER REBOOTS
```

## IGD Controller Logic (Runnable)

```
Start(ctx):
  1. Discover IGD via SSDP (retry with exponential backoff: 2s → 60s)
  2. Create/update InternetGatewayDevice CR named "default"
  3. Every pollingInterval seconds:
     a. If not discovered → re-discover (backoff), set Reachable=False
     b. GetStatus → update CRD status fields
     c. GetTrafficStats → update Prometheus gauges only
     d. ListPortMappings("upnp-nat-controller/") → update CRD status.portMappings
     e. Patch IGD status subresource
```

## UPnP Client Interface

```go
type Client interface {
    Discover(ctx context.Context) error
    IsDiscovered() bool
    GetStatus(ctx context.Context) (*DeviceStatus, error)
    GetTrafficStats(ctx context.Context) (*TrafficStats, error)
    ListPortMappings(ctx context.Context, descriptionPrefix string) ([]PortMapping, error)
    GetPortMapping(ctx context.Context, externalPort uint16, protocol string) (*PortMapping, error)
    AddPortMapping(ctx context.Context, mapping PortMapping) error
    DeletePortMapping(ctx context.Context, externalPort uint16, protocol string) error
}
```

- Thread-safe (mutex-protected)
- Rate-limited (golang.org/x/time/rate, default 1 req/s burst 3)
- Tries WANIPConnection2, falls back to WANIPConnection1, then WANPPPConnection1
- On network error: sets `discovered=false`, IGD Controller re-discovers on next tick

## SOAP Error Handling

| Code | Meaning | Action |
|------|---------|--------|
| 713 | ArrayIndexInvalid | Normal end-of-list for GetGenericPortMappingEntry |
| 714 | NoSuchEntryInArray | Mapping doesn't exist — normal for GetSpecificPortMappingEntry |
| 718 | ConflictInMappingEntry | Port owned by another device — Warning event, skip |
| 501 | ActionNotAuthorized | Router denied — Warning event, don't retry |
| Network error | Router unreachable | Mark undiscovered, IGD Controller retries |

## Prometheus Metrics

```
upnp_igd_uptime_seconds{device}          # Router uptime
upnp_igd_external_ip_info{ip}            # Info metric
upnp_igd_traffic_bytes_total{direction}  # From WANCommonInterfaceConfig
upnp_igd_traffic_packets_total{direction}
upnp_port_mappings_total                 # Active owned mappings
upnp_port_mappings_desired               # Desired mappings from annotations
upnp_soap_requests_total{method}         # SOAP call counter
upnp_soap_errors_total{method,code}      # SOAP error counter
upnp_soap_request_duration_seconds{method} # Histogram
upnp_igd_discovered                      # 1/0 gauge
```

## Health Probes

- **Liveness** (`/healthz`): built-in ping — process alive
- **Readiness** (`/readyz`): IGD discovered check — `upnpClient.IsDiscovered()`
- **Startup probe**: failureThreshold=30, period=10s (5 min for slow SSDP)

## Deployment Changes

### Dockerfile
Multi-stage: `golang:1.23` builder → `gcr.io/distroless/static:nonroot` (65532:65532)

### RBAC additions
- `gateway.nashes.uk` → `internetgatewaydevices` + `/status` (full CRUD)
- `coordination.k8s.io` → `leases` (leader election)
- `""` → `services` gets `update` verb added (for finalizers)

### Deployment template
- Add `--leader-elect`, `--metrics-bind-address=:8080`, `--health-probe-bind-address=:8081`
- Add liveness, readiness, startup probes
- Add resource requests: 10m CPU, 64Mi memory
- Keep `hostNetwork: true`

### New: metrics Service + optional ServiceMonitor

## Implementation Order

0. **Save design doc**: copy this plan to `docs/plans/2026-02-25-go-conversion-design.md` in the repo and commit
1. **Scaffold**: `go mod init`, directory structure, Makefile
2. **CRD types**: `api/v1alpha1/` types, run `controller-gen` for DeepCopy + CRD YAML
3. **UPnP client**: `internal/upnp/` — interface, discovery with backoff, port mapping CRUD
4. **Annotation parser**: `internal/annotations/` — parsing + validation with unit tests
5. **Metrics**: `internal/metrics/` — register all Prometheus metrics
6. **IGD Controller**: `internal/controller/igd_controller.go` — Runnable, discovery loop, status polling
7. **Service Reconciler**: `internal/controller/service_controller.go` — core reconciliation
8. **main.go**: wire everything, flags, health probes
9. **Dockerfile**: multi-stage build
10. **Helm chart**: updated CRD, RBAC, deployment, metrics service
11. **CI**: update GitHub Actions for Go

## Verification

1. **Unit tests**: annotation parser, UPnP client (with mock), reconciler logic
2. **Build**: `go build ./cmd/...` succeeds, `go vet`, `golangci-lint run`
3. **CRD generation**: `controller-gen` produces valid CRD YAML
4. **Docker build**: multi-arch image builds for amd64/arm64
5. **Manual test**: deploy to cluster with a UPnP router, create annotated LoadBalancer service, verify:
   - IGD CRD created with correct status
   - Port mappings appear on router
   - Reboot router → mappings re-created within 30s
   - Delete service → mappings removed from router
   - `kubectl get igd` shows device info
   - Prometheus metrics exposed on :8080/metrics
