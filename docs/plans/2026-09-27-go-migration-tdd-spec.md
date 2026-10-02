# UPnP NAT Controller — Go Migration TDD Spec

Status: Draft · 2026-09-27
Builds on: [`2026-02-25-go-conversion-design.md`](2026-02-25-go-conversion-design.md) (the "Feb design").
Where this spec and the Feb design disagree, this spec wins; each delta is listed in §12.

## 1. Purpose

Replace the Python/kopf controller in `src/` with a Go/controller-runtime controller that:

1. keeps UPnP port mappings present on the router **continuously** (level-triggered, not edge-triggered),
2. survives router restarts, UPnP daemon restarts, WAN/PPPoE reconnects and lease expiry without a pod restart,
3. handles any number of annotated Services, TCP and UDP,
4. is built test-first, with every known failure mode captured as a regression test before the fix exists.

## 2. Evidence: current failure modes

Line references are to `src/` at `2ee3a6f` (GitHub `main`). The local checkout is at `a554ecc`, one commit behind.

| # | Defect | Where | Observable effect |
|---|--------|-------|-------------------|
| D1 | Router handle discovered once and cached | `igd.py:74`, `services.py:26` (`svc=get_svc()` stored on the LB object) | After miniupnpd restarts on a new HTTP port / location, every SOAP call targets a dead URL until the pod restarts |
| D2 | Only recovery path crashes | `igd.py:103` catches `urllib.error.URLError`; `urllib` never imported (`igd.py:1-14`) | `NameError` instead of rediscovery |
| D3 | Recovery refreshes the wrong handle | `igd.py:104` replaces `memo["igd"]`; re-advertise uses `memo.lb.svc` (`igd.py:171` → `loadbalancer.py:48`) | Even a fixed D2 would not heal mappings |
| D4 | Re-add is edge-triggered | `ensurePortMappings` is `kopf.on.field(... 'status.portMappings')` (`igd.py:167`) | Fires only when the status list *changes*. After a reboot the list goes `[…] → []` once; if that single attempt fails, nothing retries. If the `portMappings` timer itself fails, the handler never fires |
| D5 | Lease expiry | `NewLeaseDuration=0` (`loadbalancer.py:56`); miniupnpd stored it as a 7-day lease (observed `leaseDuration: 604071` in `kubectl get igd`) | Mappings silently expire once D4 stops renewals |
| D6 | Single LB slot | `memo["lb"]` (`services.py:29`, `:51`; `igd.py:169-171`) | Only the last-seen Service is ever re-advertised |
| D7 | Delete handler crashes | `delete_lb` uses `memo` (`services.py:66`) without taking it as a parameter (`services.py:56`) | `NameError` on delete; kopf finalizer `advertise.upnp/kopf-finalizer` (`controller.py:6`) likely blocks Service deletion (not reproduced) |
| D8 | UDP-only Services ignored | `"when": advertise_tcp or advertise_udp` (`const.py:7`) evaluates to `advertise_tcp` | `udp.advertise.upnp/*`-only Services never handled |
| D9 | No-router crash | `get_device()` returns `None` (`utils.py:22-23`) then `IGD(None)` / `device.get_services()` | `AttributeError` at startup or mid-run |
| D10 | Ports are strings | `get_ports` returns `split(",")` (`utils.py:44`) | Passed straight to SOAP as strings; no validation |
| D11 | No liveness | `helm/templates/deployment.yaml:40-47` commented out | Wedged controller stays `Running` |
| D12 | Chart RBAC incomplete | `helm/templates/rbac.yaml:12-27` has no `crd.nashes.uk` rules; live ClusterRole has them (drift, origin unknown) | A clean chart install cannot manage its own CRD |
| D13 | Silent no-op on non-LB Services | Filter `spec.type == LoadBalancer` (`const.py:5-6`) | `media/plex` is `ClusterIP` with `tcp.advertise.upnp/enabled: "true"` and gets no mapping and no warning |

Operational context: the controller runs against OPNsense miniupnpd (`friendlyName: OPNsense UPnP IGD & PCP`), configured in "IGDv1 (IPv4 only)" compatibility mode, while the code searches for `urn:schemas-upnp-org:device:InternetGatewayDevice:2` (`utils.py:22`). Discovery must not depend on the device type version (FR-DISC-2).

## 3. Goals and non-goals

**Goals**
- G1: Mappings converge to the desired set within one reconcile interval (default 30 s) of any disruption.
- G2: No pod restart needed for any router-side event.
- G3: Every defect D1–D13 has a failing test before its fix and a passing test after.
- G4: Drop-in annotation compatibility (existing Services keep working without edits).
- G5: Safe cutover from the Python controller, including kopf finalizer removal.

**Non-goals**
- NAT-PMP / PCP clients (UPnP IGD only; PCP may follow later).
- IPv6 pinholes (`WANIPv6FirewallControl`).
- Multiple routers / multi-IGD selection beyond "explicit URL or first discovered".
- Mapping to pod IPs (Services only).

## 4. Requirements

Each requirement has an ID; the TDD plan (§8) references these IDs in test names/comments.

### 4.1 Annotations (`internal/annotations`)

| ID | Requirement |
|----|-------------|
| FR-ANN-1 | Honour existing keys: `tcp.advertise.upnp/enabled`, `tcp.advertise.upnp/ports`, `udp.advertise.upnp/enabled`, `udp.advertise.upnp/ports`. `enabled` is true only for the exact string `"true"` (case-insensitive accepted, documented). |
| FR-ANN-2 | `ports` is a comma-separated list; whitespace trimmed; each entry is `N` (external = internal = N) or `EXT:INT` (new). Ports are integers 1–65535. |
| FR-ANN-3 | Duplicate `(protocol, externalPort)` within one Service is a validation error. |
| FR-ANN-4 | `INT` must match a `spec.ports[].port` of the same protocol on the Service; otherwise validation error (the router forwards to the LB IP on that port). |
| FR-ANN-5 | Optional `advertise.upnp/lease-seconds` (new): requested lease, integer ≥ 0; default from flag `--lease-duration` (default 3600). |
| FR-ANN-6 | Parse result is a sorted, deduplicated `[]DesiredMapping`; parse errors are aggregated (all reported, not just the first). |

### 4.2 Service reconciliation (`internal/controller/service_controller.go`)

| ID | Requirement |
|----|-------------|
| FR-SVC-1 | Watches all Services cluster-wide. A Service is **managed** when any `*.advertise.upnp/enabled == "true"`. |
| FR-SVC-2 | Managed + `type: LoadBalancer` + IPv4 ingress IP → desired mappings = parsed annotations targeting `status.loadBalancer.ingress[0].ip` (first IPv4 entry; hostname-only ingress → Warning event `NoIPv4Ingress`, requeue). |
| FR-SVC-3 | Managed but not `LoadBalancer` → Warning event `NotLoadBalancer`, no mappings, no requeue until spec change (fixes D13 visibility). |
| FR-SVC-4 | LB IP not yet assigned → requeue after 5 s, no event spam (one Normal event `WaitingForIP`). |
| FR-SVC-5 | Ownership is by description `upnp-nat-controller/<namespace>/<name>`, never by internal IP. |
| FR-SVC-6 | For each desired mapping: missing → Add; ours + matches → renew if remaining lease < 2 × reconcile interval (else no-op); ours + differs (IP/internal port) → Delete + Add; not ours → Warning event `PortConflict`, condition on the Service's status is not used (events only), continue with other mappings. |
| FR-SVC-7 | Stale owned mappings (router has our description for this Service, not in desired set) → Delete. |
| FR-SVC-8 | Every successful reconcile returns `RequeueAfter: --resync-interval` (default 30 s). This is the level-triggered loop that fixes D4/D5. |
| FR-SVC-9 | Finalizer `upnp.nashes.uk/port-mappings` added to managed Services. On deletion, or when a Service stops being managed (annotations removed, type changed), delete owned mappings then remove the finalizer. If the router is unreachable during deletion, retry with backoff up to `--finalizer-timeout` (default 10 m), then remove the finalizer anyway with a Warning event `OrphanedMappings` (never block Service deletion forever). |
| FR-SVC-10 | Legacy cleanup: remove finalizer `advertise.upnp/kopf-finalizer` and annotations `advertise.upnp/kopf-managed`, `advertise.upnp/last-handled-configuration` from any Service that carries them (live: `traefik/public-traefik`). Idempotent. |
| FR-SVC-11 | Legacy mappings: a mapping whose description is exactly `<namespace>/<name>` (Python format, `loadbalancer.py:55`) and matches a managed Service is adopted (deleted + re-added with the new description) — prevents a `PortConflict` against our own old mapping during cutover. |
| FR-SVC-12 | Router unreachable → reconcile returns an error-free `RequeueAfter` (short, 10 s) and sets nothing destructive; never deletes owned state based on a failed read. |

### 4.3 Discovery and router client (`internal/upnp`)

| ID | Requirement |
|----|-------------|
| FR-DISC-1 | `--igd-url` flag: if set, skip SSDP and build clients from that root-device URL (`NewWANIPConnection2ClientsByURLCtx` etc. in goupnp). |
| FR-DISC-2 | Otherwise SSDP-search both `InternetGatewayDevice:1` and `:2`; pick the first device exposing a connection service, preferring `WANIPConnection2` → `WANIPConnection1` → `WANPPPConnection1`. |
| FR-DISC-3 | Discovery retries with exponential backoff 2 s → 60 s (jittered) and never crashes the process (fixes D9). |
| FR-DISC-4 | The client holds **no long-lived SOAP handle across failures**: any transport error (dial refused, timeout, 404 on control URL) invalidates the cached clients; the next call re-discovers (fixes D1–D3). |
| FR-DISC-5 | Router restart detection: if `uptime` decreases, the device location URL changes, or external IP changes between polls, emit a `RouterRestarted`/`ExternalIPChanged` event on the IGD CR and trigger an immediate resync of all managed Services (don't wait for the 30 s tick). |
| FR-DISC-6 | All SOAP calls take a `context.Context` with a per-call timeout (`--soap-timeout`, default 5 s) and are rate-limited (default 5 req/s, burst 10). |
| FR-DISC-7 | SOAP fault mapping to typed errors: 713 `ErrEndOfList`, 714 `ErrNoSuchEntry`, 718 `ErrConflict`, 606 `ErrNotAuthorized`, 501 `ErrActionFailed` (transient), 725 `ErrOnlyPermanentLeases` (retry once with lease 0), 724 `ErrSamePortRequired`, 727 `ErrExternalPortWildcard`; transport errors wrap `ErrUnreachable`. 713/714/718 are used by the current code (`igd.py:161`, `loadbalancer.py:59`, `:102`); the others are from memory of the UPnP IGD WANIPConnection spec and **must be confirmed against the spec document in M3** before being encoded in tests. |

### 4.4 IGD status resource (`api/v1alpha1`, `internal/controller/igd_controller.go`)

| ID | Requirement |
|----|-------------|
| FR-IGD-1 | Cluster-scoped `InternetGatewayDevice` in group `gateway.nashes.uk/v1alpha1`, singleton named `default`, created by the controller (spec from Feb design: `pollingInterval`, default 30, min 10). |
| FR-IGD-2 | Status: `friendlyName`, `location`, `internalIP`, `externalIP`, `connectionStatus`, `uptime`, `lastSeen`, `portMappings` (owned only, with `serviceRef`), `conditions`. |
| FR-IGD-3 | Conditions: `Discovered`, `Reachable`, `Connected` (router reports `ConnectionStatus == "Connected"`), `PublicExternalIP` (False when external IP is RFC 1918, CGNAT `100.64.0.0/10` or empty → double-NAT warning), `Ready` (= Discovered ∧ Reachable ∧ Connected). |
| FR-IGD-4 | Traffic counters go to Prometheus only (Feb design); status writes only when a field other than `lastSeen` changes, or `lastSeen` is older than 5 m (limits etcd churn). |

### 4.5 Operations

| ID | Requirement |
|----|-------------|
| NFR-OPS-1 | `/healthz` fails if the resync loop has not completed a pass in 5 × resync interval (detects a wedged controller, D11). It does **not** fail because the router is down (restarting the pod cannot fix the router). |
| NFR-OPS-2 | `/readyz` = manager started + informer caches synced. |
| NFR-OPS-3 | Prometheus metrics per Feb design, plus `upnp_reconcile_total{result}`, `upnp_router_restarts_total`, `upnp_port_conflicts_total`, `upnp_mapping_drift_repairs_total` (mappings re-added because they were missing). |
| NFR-OPS-4 | Structured logs (`logr`/zap), one line per mapping change; never log at info level on no-op passes. |
| NFR-OPS-5 | Graceful shutdown: SIGTERM stops new reconciles; does **not** delete mappings. |
| NFR-OPS-6 | Leader election on by default (`coordination.k8s.io/leases`), replicas 1. |
| NFR-OPS-7 | `hostNetwork: true` kept (SSDP multicast needs the node's LAN interface). |
| NFR-SEC-1 | Distroless static nonroot image, read-only root FS, no shell. |
| NFR-SEC-2 | Controller only talks to the discovered/configured router; the device-description and control URLs must resolve to the same host as the SSDP responder (or `--igd-url` host), otherwise reject (guards against a LAN device advertising a control URL elsewhere). |

## 5. Architecture

```
cmd/main.go                     flags, manager, wiring
api/v1alpha1/                   InternetGatewayDevice types (+ generated deepcopy/CRD)
internal/annotations/           pure parsing/validation (no k8s client)
internal/mapping/               pure diff: Plan(desired, actual, now) -> []Action
internal/upnp/
  client.go                     Client interface + goupnp-backed implementation
  discovery.go                  SSDP / by-URL, backoff, invalidation
  errors.go                     SOAP fault -> typed errors
  fake/                         in-memory fake Client (unit tests)
  fakeigd/                      httptest-based fake router speaking real SOAP (component tests)
internal/controller/
  service_controller.go         Reconciler for v1.Service
  igd_controller.go             Runnable: poll, restart detection, CR status, resync trigger
  resync.go                     channel source that enqueues all managed Services
internal/metrics/
internal/health/                liveness tracker (last successful pass)
```

Key design rule for testability: **the reconciler never calls goupnp directly.** It calls `internal/mapping.Plan` (pure) and executes actions against the `upnp.Client` interface. Time comes from an injected `clock.Clock` (`k8s.io/utils/clock/testing` in tests).

```go
type Client interface {
    Status(ctx context.Context) (DeviceStatus, error)          // name, location, externalIP, connStatus, uptime
    Traffic(ctx context.Context) (TrafficStats, error)
    List(ctx context.Context) ([]PortMapping, error)          // all entries via GetGenericPortMappingEntry until 713
    Get(ctx context.Context, proto Protocol, ext uint16) (PortMapping, error) // ErrNoSuchEntry when absent
    Add(ctx context.Context, m PortMapping) error
    Delete(ctx context.Context, proto Protocol, ext uint16) error
    Invalidate()                                              // drop cached SOAP clients
}
```

`mapping.Plan(desired []Desired, actual []PortMapping, owner string, renewBelow time.Duration) []Action` returns `Add`, `Renew`, `Replace`, `Delete`, `Conflict` actions. Everything interesting about correctness lives here and is tested without any I/O.

## 6. Test strategy

| Layer | Scope | Tooling | Runs in |
|-------|-------|---------|---------|
| L1 Unit | `annotations`, `mapping`, `upnp/errors`, health tracker, IP classification | `testing`, table-driven, `go-cmp` | `go test ./...` (< 5 s) |
| L2 Component | `upnp` client against **fakeigd** (real HTTP + SOAP) | `httptest`, goupnp by-URL constructors | `go test ./...` |
| L3 Controller | Reconcilers against a real API server + fake `upnp.Client` or fakeigd | `sigs.k8s.io/controller-runtime/pkg/envtest` | `make test` (downloads envtest assets) |
| L4 Acceptance | Real OPNsense router, manual checklist (§10) | kubectl, router UI | Before cutover |

### 6.1 fakeigd

An `httptest.Server` that serves a root device description (IGD:1 or IGD:2, configurable) and a SOAP control endpoint implementing miniupnpd-like semantics:

- in-memory mapping table keyed by `(proto, externalPort)`; `GetGenericPortMappingEntry` returns 713 past the end; `GetSpecificPortMappingEntry` returns 714 when absent; `AddPortMapping` for an existing key with a different `InternalClient` returns 718;
- lease handling: lease 0 is stored as 604800 (observed miniupnpd behaviour), remaining lease decrements with an injected clock, expired entries vanish;
- fault injection: `Restart()` (clear table, reset uptime, **move to a new port** — i.e. a new URL), `Stop()`/`Start()`, per-action forced faults, response delay (timeouts), `SetExternalIP()`, `SetConnectionStatus()`;
- request log for assertions (e.g. "exactly one Add after restart").

fakeigd is itself test-driven (L2 tests assert it returns the SOAP faults goupnp expects) so the higher layers can trust it.

### 6.2 Conventions

- Test names reference requirement or defect IDs: `TestPlan_OursDifferentIP_Replaces /* FR-SVC-6 */`, `TestReconcile_RouterRestartOnNewPort_Recovers /* D1 */`.
- Red → green → refactor. A PR that adds behaviour without a test that failed first is incomplete; the PR description lists the test(s) that were red.
- Table-driven tests for all pure packages; no sleeps in tests (inject clock; use `Eventually` with envtest only).
- Coverage gate: `internal/annotations`, `internal/mapping`, `internal/upnp` ≥ 90 % statements; controllers ≥ 75 %.
- `go vet`, `golangci-lint run`, `go test -race ./...` must pass in CI.

## 7. Regression scenario catalogue

These are the end-to-end behaviours the controller must guarantee. Each becomes an L3 test (envtest + fakeigd) in milestone M7; the ID is the test name suffix.

| ID | Given | When | Then |
|----|-------|------|------|
| S1 RouterReboot | Service mapped (443/TCP) | fakeigd `Restart()` (table cleared, same port) | mapping re-added within one resync interval; `upnp_mapping_drift_repairs_total` +1 |
| S2 RouterReboot_NewPort | Service mapped | `Restart()` moves control URL to a new port | client invalidates, re-discovers, re-adds; no pod restart (D1–D3) |
| S3 RouterDownAtBoot | fakeigd stopped | controller starts; router starts 90 s later | no crash; `Discovered=False` then `True`; mapping added (D9) |
| S4 LeaseExpiry | lease 3600 | clock advanced past half-lease | `Renew` issued before expiry; mapping never absent (D5) |
| S5 MultipleServices | two annotated LBs | router restart | both Services' mappings restored (D6) |
| S6 UDPOnly | Service with only `udp.*` annotations | create | UDP mapping added (D8) |
| S7 DeleteService | mapped Service | delete | mappings deleted, finalizer removed, Service gone (D7) |
| S8 DeleteWhileRouterDown | mapped Service, router stopped | delete | retries; after `--finalizer-timeout` finalizer removed + `OrphanedMappings` event |
| S9 Conflict | router already maps 443 → other host (different description) | create Service wanting 443 | `PortConflict` event, other ports still mapped, foreign mapping untouched |
| S10 LBIPChange | mapped to 172.16.1.2 | LB IP changes to 172.16.1.9 | Replace action; router points to new IP |
| S11 AnnotationRemoved | mapped | remove `enabled` annotation | mappings deleted, finalizer removed |
| S12 PortListChanged | ports `443,32400` | change to `443` | 32400 deleted, 443 untouched (no churn) |
| S13 NotLoadBalancer | ClusterIP with annotations (like `media/plex`) | create | `NotLoadBalancer` Warning, no router calls (D13) |
| S14 ExternalIPChanged | PPPoE reconnect | fakeigd `SetExternalIP()` | `ExternalIPChanged` event, immediate resync, status updated |
| S15 LegacyCutover | Service with `advertise.upnp/kopf-finalizer` + router mapping described `traefik/public-traefik` | controller starts | kopf finalizer/annotations removed; mapping adopted with new description; no `PortConflict` (FR-SVC-10/11) |
| S16 DoubleNAT | fakeigd external IP `192.168.1.10` | poll | `PublicExternalIP=False` condition with message |
| S17 Wedged | resync loop blocked (test hook) | 5 × interval | `/healthz` returns 500 (D11) |
| S18 NoFlapOnReadFailure | mapped Service | `List`/`Get` fail with transport error | no Delete issued; requeue short (FR-SVC-12) |

## 8. TDD implementation plan

Each milestone: write the listed tests first (they must fail/compile-fail), implement until green, refactor, commit. One PR per milestone. Test lists are the minimum; add cases as bugs are found.

### M0 — Scaffold (no behaviour)
- `go.mod` (module `github.com/nashant/upnp-nat-controller`, Go 1.24 to match the local toolchain), `Makefile` targets: `test`, `test-unit`, `lint`, `generate`, `manifests`, `build`, `docker-build`, `envtest`.
- CI workflow on PRs: `go vet`, `golangci-lint`, `go test -race ./...`, coverage report. Image push only from `main` / tags.
- Keep `src/` untouched until M9.
- Exit: `make test lint` green on an empty test suite.

### M1 — Annotation parser (`internal/annotations`) — FR-ANN-*
Tests first:
- `TestParse_TCPEnabledSinglePort`, `TestParse_UDPOnly /* D8 */`, `TestParse_BothProtocols`
- `TestParse_DisabledOrMissing_ReturnsNone`, `TestParse_EnabledNotExactlyTrue`
- `TestParse_PortsWhitespaceAndOrder_SortedDeduped`
- `TestParse_ExtIntSyntax`, `TestParse_PortOutOfRange`, `TestParse_NonNumeric /* D10 */`
- `TestParse_DuplicateExternalPort_Error`, `TestParse_InternalPortNotOnService_Error`
- `TestParse_MultipleErrors_Aggregated`, `TestParse_LeaseAnnotation_DefaultAndOverride`
- `TestIsManaged_TCPOrUDP /* D8 */`
- Fuzz: `FuzzParsePorts` (never panics, errors or valid ports only).

### M2 — Mapping planner (`internal/mapping`) — FR-SVC-5/6/7/11
Tests first (table-driven over `desired × actual`):
- `TestPlan_Missing_Adds`, `TestPlan_OursMatching_NoOp`, `TestPlan_OursMatching_LeaseLow_Renews`
- `TestPlan_OursDifferentIP_Replaces`, `TestPlan_OursDifferentInternalPort_Replaces`
- `TestPlan_Foreign_Conflict`, `TestPlan_ForeignDoesNotBlockOtherPorts`
- `TestPlan_StaleOwned_Deletes`, `TestPlan_OtherServiceOwned_Untouched`
- `TestPlan_LegacyDescription_Adopts /* FR-SVC-11 */`
- `TestPlan_Deterministic_Order`
- `TestOwnerDescription_Format`, `TestParseOwnerDescription_RoundTrip`

### M3 — SOAP errors + fakeigd (`internal/upnp/errors`, `internal/upnp/fakeigd`)
Tests first:
- `TestClassify_SOAPFaultCodes` (713/714/718/606/501/725/724/727 — codes confirmed against the IGD spec first, see FR-DISC-7), `TestClassify_TransportErrorsAreUnreachable`
- fakeigd contract tests via goupnp: `TestFakeIGD_GenericEntry_713AtEnd`, `TestFakeIGD_Specific_714WhenAbsent`, `TestFakeIGD_Add_718OnForeignConflict`, `TestFakeIGD_LeaseZeroStoredAs604800`, `TestFakeIGD_LeaseExpiry`, `TestFakeIGD_RestartClearsAndMovesPort`, `TestFakeIGD_IGDv1AndV2Descriptions`

### M4 — UPnP client (`internal/upnp`) — FR-DISC-*
Tests first (against fakeigd):
- `TestClient_ByURL_PrefersWANIP2ThenIP1ThenPPP /* FR-DISC-2 */`
- `TestClient_ListAddGetDelete_RoundTrip`
- `TestClient_TransportError_Invalidates_NextCallRediscovers /* D1–D3 */`
- `TestClient_RestartOnNewPort_RecoversWithoutRecreate /* D1 */`
- `TestClient_PerCallTimeout /* FR-DISC-6 */`, `TestClient_RateLimited`
- `TestClient_OnlyPermanentLeases_RetriesWithZero /* 725 */`
- `TestClient_RejectsControlURLOnDifferentHost /* NFR-SEC-2 */`
- `TestDiscovery_BackoffSchedule_2sTo60s` (fake clock), `TestDiscovery_NoDevice_ReturnsErrorNotPanic /* D9 */`
- SSDP itself is behind a `Searcher` interface; the real implementation gets one smoke test guarded by `-tags integration`.

### M5 — CRD types (`api/v1alpha1`) — FR-IGD-1/2/3
- Types + kubebuilder markers; `make generate manifests` produces deepcopy + CRD YAML into `helm/crds/`.
- Tests first: `TestCRD_ValidatesPollingIntervalMin` (envtest: create with 5 → rejected), `TestCRD_StatusSubresource`, `TestClassifyExternalIP_PrivateCGNATPublic /* FR-IGD-3 */` (pure).

### M6 — IGD controller (`internal/controller/igd_controller.go`) — FR-IGD-*, FR-DISC-5
Tests first (envtest + fake `upnp.Client`, fake clock):
- `TestIGD_CreatesDefaultCR`, `TestIGD_StatusFieldsPopulated`
- `TestIGD_ConditionsTransition_DiscoveredReachableConnectedReady`
- `TestIGD_UptimeDecrease_EmitsRouterRestarted_TriggersResync`
- `TestIGD_ExternalIPChange_EmitsEvent_TriggersResync`
- `TestIGD_StatusNotRewrittenWhenUnchanged /* FR-IGD-4 */`
- `TestIGD_TrafficOnlyInMetrics`

### M7 — Service reconciler (`internal/controller/service_controller.go`) — FR-SVC-*
Tests first (envtest + fake `upnp.Client` for fine-grained cases, fakeigd for S-scenarios):
- `TestReconcile_UnmanagedService_NoRouterCalls`
- `TestReconcile_NotLoadBalancer_WarningEvent /* D13, S13 */`
- `TestReconcile_NoIngressIP_RequeuesAfter5s`
- `TestReconcile_AddsFinalizerBeforeFirstMapping`
- `TestReconcile_Success_RequeueAfterResyncInterval /* D4 */`
- `TestReconcile_ReadFailure_NoDeletes_ShortRequeue /* S18 */`
- `TestReconcile_RemovesLegacyKopfFinalizerAndAnnotations /* FR-SVC-10 */`
- `TestReconcile_Delete_RemovesMappingsThenFinalizer /* D7 */`
- `TestReconcile_Delete_RouterDown_TimeoutReleasesFinalizer /* S8 */`
- Then all of §7 S1–S16, S18 as `TestScenario_<ID>`.

### M8 — Wiring, health, metrics (`cmd/main.go`, `internal/health`, `internal/metrics`)
Tests first:
- `TestHealth_FailsAfter5xIntervalWithoutPass /* S17, NFR-OPS-1 */`, `TestHealth_RouterDownStillHealthy`
- `TestMetrics_Registered` (names/labels as in §4.5), `TestMetrics_DriftRepairCounted`
- `TestFlags_Defaults` (resync 30 s, lease 3600, soap-timeout 5 s, finalizer-timeout 10 m)
- `TestMain_ResyncSourceEnqueuesAllManaged` (envtest)

### M9 — Packaging and chart
- Dockerfile: multi-stage `golang:1.24` → `gcr.io/distroless/static:nonroot`.
- Chart: new CRD (`gateway.nashes.uk`), RBAC (services get/list/watch/update/patch, services/finalizers update, events create/patch, `gateway.nashes.uk` IGD + status, leases), probes (`/healthz`, `/readyz`, startup probe 30 × 10 s), metrics Service, optional ServiceMonitor, resources 10m/64Mi, `hostNetwork: true`, flags as values.
- Chart version → `0.1.0`, `appVersion` = image tag. Image tags: `vX.Y.Z` + `sha-<short>`; stop deploying `:latest` (`helm/values.yaml:9`, `.github/workflows/main.yml:35`).
- Tests first: `helm lint`; `helm template | kubeconform`; a golden-file test for rendered RBAC including the verbs above (catches D12-style drift).
- Remove `src/`, Python Dockerfile, old CRD `helm/crds/igd.yaml` in this milestone.

## 9. Cutover plan (home cluster)

Revised 2026-10-01. Flux ignores `spec.chart.spec.version` for `GitRepository` sources. What deploys is the `version` in `helm/Chart.yaml` on `main`, so bumping that version is the deploy and reverting it is the rollback. Helm does not install CRDs on upgrade.

1. Merge M0–M9. release-please opens a release PR; merging it tags `v0.1.0`, and CI pushes `ghcr.io/nashant/upnp-nat-controller:0.1.0` (the chart `appVersion`). Make the GHCR package public, or configure image pull credentials in the cluster.
2. In the home repo's `clusters/prod/platform/upnp-nat-controller/helmrelease.yaml`, set `install.crds` and `upgrade.crds` to `CreateReplace` and add `upgrade.remediation`. Then run `flux reconcile source git upnp-nat-controller` and confirm the chart artifact is `0.1.0` before the HelmRelease is resumed or reconciled.
3. On rollout the new controller:
   - strips the kopf finalizer and annotations (S15);
   - adopts the `traefik/public-traefik` mappings, rewriting them as `unc/traefik/public-traefik`;
   - creates `internetgatewaydevices.gateway.nashes.uk/default`.
4. Verify §10. Then delete the old CRD `internetgatewaydevices.crd.nashes.uk` and its CR (Helm does not delete CRDs from `crds/`).
5. Rollback:
   1. Suspend the HelmRelease and `helm rollback` to the previous revision, or revert the merge on `main`.
   2. Remove the finalizer `upnp.nashes.uk/port-mappings` from managed Services. The Python controller doesn't know it, so deletes would hang.
   3. The Python controller re-adds its kopf finalizer on resume. It judges ownership by internal IP (`loadbalancer.py:42`), so a same-IP mapping described `unc/...` falls through to `AddPortMapping` (`loadbalancer.py:47-57`) and is rewritten with its `<ns>/<name>` description. Rollback is therefore safe for mappings.

## 10. Acceptance checklist (L4, real router)

- [ ] `kubectl get internetgatewaydevices.gateway.nashes.uk default` shows `Ready=True`, correct external IP, `PublicExternalIP=True`. While the old CRD exists, the bare `igd` short name resolves to it.
- [ ] Router UPnP status lists 443 and 32400 → `172.16.1.2`, description `unc/traefik/public-traefik`.
- [ ] Restart the UPnP service on OPNsense → mappings back within 30 s, `RouterRestarted` event, pod restart count unchanged.
- [ ] Reboot OPNsense → same.
- [ ] Toggle PPPoE → `ExternalIPChanged` (if IP changes), mappings present.
- [ ] Remove the annotation from a test LB → mapping removed; re-add → mapping back.
- [ ] Delete a test LB → mapping removed, Service deletion not blocked.
- [ ] `media/plex` shows a `NotLoadBalancer` Warning event.
- [ ] `/metrics` exposes the metric names in §4.5.

## 11. Open questions

1. Keep the singleton IGD CR named `default`, or name it after the discovered device UDN to allow multi-router later?
2. Should `PortConflict` optionally **take over** foreign mappings via an opt-in annotation (`advertise.upnp/force: "true"`)?
3. Default lease: 3600 s with renewal (this spec) vs lease 0 (router max, 604800 on miniupnpd). Shorter leases self-clean if the controller disappears; longer ones survive a controller outage.
4. Is PCP support (miniupnpd advertises "IGD & PCP") worth a follow-up spec?

## 12. Deltas from the Feb design

| Area | Feb design | This spec | Why |
|------|-----------|-----------|-----|
| Process | Implementation order, tests at the end ("Verification") | Test-first milestones, requirement-ID-tagged tests, coverage gates | TDD requirement |
| Discovery | IGD:2 → fallback services | IGD:1 **and** :2 search, plus `--igd-url` override | Router runs in IGDv1 compat mode |
| Stale handles | "On network error set discovered=false" | Explicit `Invalidate()` + restart detection by uptime/location/external IP + immediate resync | D1–D3, faster than 30 s |
| Leases | Not specified | Explicit lease + renewal below 2 × interval | D5 |
| Annotations | Existing keys | Adds `EXT:INT` ports, lease annotation, validation against `spec.ports` | Missing features |
| Non-LB Services | Filtered out silently | `NotLoadBalancer` Warning event | D13 (`media/plex`) |
| Deletion | Delete mappings | Finalizer with timeout; never blocks forever | D7 |
| Cutover | Not specified | Kopf finalizer/annotation strip + legacy description adoption | Live `traefik/public-traefik` carries `advertise.upnp/kopf-finalizer` |
| Liveness | Built-in ping | Fails when the resync loop is wedged; not on router outage | D11 |
| Double NAT | Not specified | `PublicExternalIP` condition | Previous WAN setup was behind `192.168.1.1` |
| Image | `golang:1.23`, `:latest` | `golang:1.24` (matches `go.mod`), versioned tags | Toolchain match, reproducible rollouts |
