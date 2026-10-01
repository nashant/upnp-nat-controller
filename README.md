# upnp-nat-controller

A Kubernetes controller that keeps UPnP port mappings on your router for
annotated `LoadBalancer` Services. Mappings are reconciled every
`--resync-interval` (30s), so they come back on their own after a router
reboot, a UPnP daemon restart, a WAN reconnect or a lease expiry.

## Annotations

| Annotation | Meaning |
|------------|---------|
| `tcp.advertise.upnp/enabled` | `"true"` (case-insensitive) to map TCP ports |
| `tcp.advertise.upnp/ports` | Comma-separated `N` or `EXT:INT`, e.g. `443,8443:443` |
| `udp.advertise.upnp/enabled` | `"true"` to map UDP ports |
| `udp.advertise.upnp/ports` | As for TCP |
| `advertise.upnp/lease-seconds` | Lease to request; default `--lease-duration` (3600). `0` asks for a permanent mapping |

`INT` must be one of the Service's `spec.ports[].port` for that protocol. The
router forwards `EXT` to the first IPv4 address in
`status.loadBalancer.ingress` on port `INT`.

```yaml
apiVersion: v1
kind: Service
metadata:
  name: public-traefik
  annotations:
    tcp.advertise.upnp/enabled: "true"
    tcp.advertise.upnp/ports: "443,32400"
spec:
  type: LoadBalancer
  ports:
    - {name: https, port: 443}
    - {name: plex, port: 32400}
```

Mappings are owned by description `upnp-nat-controller/<namespace>/<name>`.
Mappings with any other description are never changed; a clash raises a
`PortConflict` event on the Service. Annotated Services that are not
`LoadBalancer` get a `NotLoadBalancer` event.

Managed Services carry the finalizer `upnp.nashes.uk/port-mappings`. On
deletion the controller removes their mappings; if the router stays
unreachable for `--finalizer-timeout` (10m) the finalizer is dropped anyway
with an `OrphanedMappings` event.

## Router status

```console
$ kubectl get igd default
NAME      DEVICE                    INTERNAL IP   EXTERNAL IP    READY   UPTIME   AGE
default   OPNsense UPnP IGD & PCP   192.168.1.1   81.2.69.142    True    86400    3d
```

Conditions: `Discovered`, `Reachable`, `Connected`, `PublicExternalIP`
(False behind double NAT or CGNAT) and `Ready`. A router restart or external
IP change emits `RouterRestarted` / `ExternalIPChanged` and resyncs every
Service immediately.

## Flags

| Flag | Default | |
|------|---------|-|
| `--igd-url` | | Router description URL; skips SSDP |
| `--resync-interval` | `30s` | |
| `--lease-duration` | `3600` | Seconds |
| `--soap-timeout` | `5s` | Per router request |
| `--finalizer-timeout` | `10m` | |
| `--rate-limit` / `--rate-burst` | `5` / `10` | Router requests per second |
| `--leader-elect` | `true` | |
| `--metrics-bind-address` / `--health-probe-bind-address` | `:8080` / `:8081` | |

The pod runs with `hostNetwork: true` so SSDP can reach the LAN.

## Development

```console
make test       # unit, component (fake router over real SOAP) and envtest suites
make lint
make generate manifests
```

Design and test plan: [docs/plans/2026-09-27-go-migration-tdd-spec.md](docs/plans/2026-09-27-go-migration-tdd-spec.md).
