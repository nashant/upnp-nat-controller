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
| `advertise.upnp/lease-seconds` | Lease to request; default `--lease-duration` (3600). `0` asks for a permanent mapping. Otherwise it must be longer than twice `--resync-interval`. Mappings are renewed at half their lease |

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

Mappings are owned by their description, `unc/<namespace>/<name>` by
default (`--description-prefix`). The router keeps at most 63 bytes of a
description, so longer ones are cut and end in `~` plus an 8-character
hash. Mappings described `upnp-nat-controller/<namespace>/<name>` (earlier
releases) or `<namespace>/<name>` (the Python controller) are adopted.
Mappings with any other description are never changed; a clash raises a
`PortConflict` event on the Service. Annotated Services that are not
`LoadBalancer` get a `NotLoadBalancer` event.

Managed Services carry the finalizer `upnp.nashes.uk/port-mappings`. On
deletion the controller removes their mappings; if the router stays
unreachable for `--finalizer-timeout` (10m) the finalizer is dropped anyway
with an `OrphanedMappings` event.

## Router status

```console
$ kubectl get internetgatewaydevices.gateway.nashes.uk default
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

The pod runs with `hostNetwork: true` so SSDP can reach the LAN. Set
`--igd-url` (chart value `controller.igdURL`) to skip SSDP, which any LAN host
can answer.

## Router prerequisites

The controller asks the router to forward to the Service's LoadBalancer IP,
not to its own (node) address. On OPNsense (Services → UPnP & NAT-PMP):

- **Allow third-party mapping** must be set to UPnP IGD. Otherwise miniupnpd
  runs in secure mode and refuses every mapping (UPnP error 606).
- If **Default deny** is on, add allow rules covering the LoadBalancer IP
  range and the external ports, including ports below 1024.

Ports the router itself listens on (web GUI, SSH, VPN) can't be mapped.

## Releases

Images are published to `ghcr.io/nashant/upnp-nat-controller`:
- each push to `main` gets a `sha-<commit>` tag;
- each release gets a `<version>` tag, with a signed provenance attestation and an SBOM.

Each release also publishes the chart to `oci://ghcr.io/nashant/charts/upnp-nat-controller`, at the same version:

```console
helm install upnp-nat-controller oci://ghcr.io/nashant/charts/upnp-nat-controller \
  --namespace upnp-nat-controller --create-namespace
```

release-please keeps a release PR open that bumps `version` and `appVersion` in `helm/Chart.yaml`. Merging it tags the release. PR titles must be Conventional Commits (`feat: …`, `fix: …`).

## Development

```console
make test       # unit, component (fake router over real SOAP) and envtest suites
make lint
make generate manifests
```
