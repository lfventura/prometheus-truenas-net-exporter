# TrueNAS Network Exporter for Prometheus

A Prometheus exporter that collects per-network-interface traffic metrics on Linux, enriched with Docker container, VM, and application labels. Purpose-built for **TrueNAS SCALE** but works on any Linux host with Docker and/or QEMU VMs.

## Why this exporter?

On a TrueNAS SCALE system, a single host can have 40+ network interfaces: physical NICs, Linux bridges, Docker container veths, VM taps, VLANs, and macvtap devices. Standard tools like `node_exporter` report raw interface names (`veth402dbe0`, `vnet3`, `br-2c852816592c`) — meaningless without cross-referencing Docker, QEMU, and sysfs. This exporter enriches every interface with:

- **What it is** (`instance_type`): physical, bridge, docker, vm, vlan, macvtap, loopback
- **Who owns it** (`instance`): container name, VM name, or interface name
- **Which application** (`app`): derived from Docker Compose project labels or Docker network names
- **Bridge membership** (`bridge`): which bridge the interface is attached to
- **Link state** (`state`): up, down, or unknown

## Features

- Reads traffic counters from `/proc/1/net/dev` (host network namespace)
- Maps Docker container veth interfaces → container names via Docker Engine API
- Maps VM vnet/macvtap interfaces → VM names via TrueNAS `midclt` API (with `virsh` fallback)
- Resolves Docker bridge interfaces → Docker network names via Networks API
- Extracts application names from Docker Compose project labels (`ix-<app>` prefix for TrueNAS apps)
- Derives `app` label for bridges and orphan veths from their Docker network name
- Classifies all interfaces: `physical`, `bridge`, `docker`, `vm`, `vlan`, `macvtap`, `loopback`

## Metrics

### Counters (from `/proc/net/dev`)

| Metric | Description |
|---|---|
| `net_interface_rx_bytes_total` | Total bytes received |
| `net_interface_tx_bytes_total` | Total bytes transmitted |
| `net_interface_rx_packets_total` | Total packets received |
| `net_interface_tx_packets_total` | Total packets transmitted |
| `net_interface_rx_errors_total` | Total receive errors |
| `net_interface_tx_errors_total` | Total transmit errors |
| `net_interface_rx_dropped_total` | Total received packets dropped |
| `net_interface_tx_dropped_total` | Total transmitted packets dropped |

All metrics are counters. Use `rate()` or `derivative()` for throughput.

### Labels

| Label | Description | Examples |
|---|---|---|
| `interface` | Host-side kernel interface name | `veth402dbe0`, `enp8s0f0`, `br0`, `vnet3` |
| `instance` | Resolved human-readable name | `ix-grafana-grafana-1`, `vyos`, `enp8s0f0` |
| `instance_type` | Interface classification | `physical`, `bridge`, `docker`, `vm`, `vlan`, `macvtap`, `loopback` |
| `app` | Application name (from Docker Compose project or network) | `grafana`, `immich`, `jellyfin` |
| `bridge` | Parent bridge (if interface is a bridge member) | `br0`, `br-2c852816592c` |
| `state` | Link state from sysfs operstate | `up`, `down`, `unknown` |

### Example Output

```
# Physical NIC
net_interface_rx_bytes_total{interface="enp8s0f0",instance="enp8s0f0",instance_type="physical",app="",bridge="",state="up"} 1.111594096922e+12

# Docker container mapped to app
net_interface_rx_bytes_total{interface="veth402dbe0",instance="ix-grafana-grafana-1",instance_type="docker",app="grafana",bridge="br-2c852816592c",state="up"} 2.56302961e+08

# Docker bridge mapped to network name with app
net_interface_rx_bytes_total{interface="br-2c852816592c",instance="ix-grafana_default",instance_type="bridge",app="grafana",bridge="",state="up"} 2.54524065e+08

# VM tap interface mapped to VM name
net_interface_rx_bytes_total{interface="vnet3",instance="vyos",instance_type="vm",app="",bridge="br0",state="unknown"} 1.115796347231e+12

# macvtap interface mapped to VM name
net_interface_rx_bytes_total{interface="macvtap0",instance="vyos",instance_type="macvtap",app="",bridge="",state="up"} 1.111594084954e+12

# System bridge
net_interface_rx_bytes_total{interface="br0",instance="br0",instance_type="bridge",app="",bridge="",state="up"} 1.343738956933e+12

# VLAN
net_interface_rx_bytes_total{interface="vlan1",instance="vlan1",instance_type="vlan",app="",bridge="br0",state="up"} 2.17320405154e+11
```

---

## How It Works — Technical Deep Dive

### TrueNAS SCALE Network Topology

A typical TrueNAS SCALE system creates a complex network topology:

```
                        ┌─────────────────────────────────────────┐
                        │              TrueNAS Host               │
                        │                                         │
  Physical NICs         │  enp8s0f0 ──┐                           │
                        │  enp8s0f1 ──┤                           │
                        │             │                           │
  System Bridges        │  br0 ───────┤ (main bridge)             │
                        │  │  ├── vlan1  (VLAN trunk)             │
                        │  │  ├── vnet0  (pihole VM)              │
                        │  │  ├── vnet1  (pritunl VM)             │
                        │  │  ├── vnet3  (vyos VM)                │
                        │  │  └── ...                             │
                        │  br1 ──── vlan2 + vnet2 + vnet5         │
                        │                                         │
  macvtap               │  macvtap0 ── vyos (direct to NIC)       │
                        │                                         │
  Docker Bridges        │  br-2c852816592c (ix-grafana_default)   │
  (one per app network) │  │  └── veth402dbe0 → ix-grafana-1      │
                        │  br-f33136af96f9 (ix-immich_net)        │
                        │  │  ├── veth8b2f7e9 → immich-ml-1       │
                        │  │  ├── vethc4b3cbf → immich-pgvecto-1  │
                        │  │  ├── veth4be5411 → immich-redis-1    │
                        │  │  └── veth3f3cbed → immich-server-1   │
                        │  ...                                    │
                        └─────────────────────────────────────────┘
```

### Step 1: Reading Interface Traffic (`/proc/1/net/dev`)

**Critical discovery**: `/proc/net/dev` is a symlink to `/proc/self/net/dev`, which resolves to the **container's own network namespace** (showing only `lo` + `eth0`). Even with `pid: host`, the container's network namespace remains unchanged.

**Solution**: Read `/proc/1/net/dev` — PID 1 (the host's init process) is always in the host's network namespace. This gives us all ~50 host interfaces.

```
/proc/net/dev       → /proc/self/net/dev → container namespace (❌ only lo, eth0)
/proc/1/net/dev     → host namespace     (✅ all 50+ interfaces)
```

### Step 2: Interface Classification

Each interface is classified using sysfs heuristics:

| Prefix/Pattern | Type | Detection Method |
|---|---|---|
| `lo` | `loopback` | Name match |
| `veth*` | `docker` | Prefix match |
| `vnet*` | `vm` | Prefix match |
| `macvtap*`, `macvlan*` | `macvtap` | Prefix match |
| `vlan*` | `vlan` | Prefix match |
| `br-*`, `br*`, `docker*`, `incus*` | `bridge` | Prefix match |
| Others with `device/driver` in sysfs | `physical` | Symlink exists at `/sys/class/net/<iface>/device/driver` |
| Everything else | `unknown` | Fallback |

Bridge membership is detected via the sysfs `master` symlink:
```
/sys/class/net/veth402dbe0/master → ../../br-2c852816592c
```

Interface state is read from:
```
/sys/class/net/<iface>/operstate → "up", "down", "unknown"
```

### Step 3: Docker Container Mapping (veth → container)

Docker creates a **veth pair** for each container: one end inside the container (`eth0`), one on the host (`vethXXXXXXX`). The host-side name is randomized and not exposed by the Docker API.

**Mapping process**:

1. Connect to Docker Engine API via Unix socket (`/var/run/docker.sock`)
2. `GET /containers/json` → list running containers
3. `GET /containers/<id>/json` → get PID, name, labels, networks
4. For each container, read the container's sysfs via `/proc/<PID>/root/sys/class/net/`:
   - List all interfaces (skip `lo`)
   - Read `iflink` for each → this is the **host-side ifindex** of the veth peer
5. Match ifindex to host interface names via `/sys/class/net/<iface>/ifindex`

```
Container PID 12345
  └── /proc/12345/root/sys/class/net/eth0/iflink → "42"
Host
  └── /sys/class/net/veth402dbe0/ifindex → "42"
  ∴ veth402dbe0 belongs to container PID 12345
```

**App name extraction**: Read the `com.docker.compose.project` label from the container. TrueNAS apps set this to `ix-<appname>`, so we strip the `ix-` prefix.

### Step 4: Docker Network Mapping (bridge → network name → app)

Docker creates one Linux bridge per Docker network, named `br-<first 12 chars of network ID>`.

**Mapping process**:

1. `GET /networks` → list all Docker networks
2. For each bridge-driver network, resolve the host bridge name:
   - From `Options["com.docker.network.bridge.name"]` if set
   - Otherwise: `br-` + first 12 chars of network ID
3. Map bridge interface → Docker network name

**App derivation from network name**: TrueNAS apps create Docker networks named `ix-<appname>_<suffix>` (e.g., `ix-grafana_default`, `ix-immich_ix-internal-immich-net`). The app name is extracted by stripping `ix-` and taking everything before the first `_`.

This provides:
- `app` label for bridge interfaces (e.g., `br-2c852816592c` → app `grafana`)
- `app` label for orphan veths (veths whose container disappeared between scrapes) — derived from their parent bridge's network name

### Step 5: VM Mapping (vnet/macvtap → VM name)

TrueNAS SCALE uses QEMU for VMs but does **not** expose a standard `libvirt` socket (`/var/run/libvirt/libvirt-sock` does not exist). The TrueNAS middleware manages VMs through its own API.

**Primary method — TrueNAS `midclt` API**:

1. Run `midclt call vm.query` (via `chroot /host` when in container)
2. Parse JSON response to get VM name + QEMU PID for each running VM
3. For each QEMU PID, scan `/proc/<PID>/fd/` to find tap/macvtap file descriptors:
   - **tap interfaces**: FD points to `/dev/net/tun` → read `/proc/<PID>/fdinfo/<FD>` for the `iff:` line which contains the interface name (e.g., `iff:\tvnet0`)
   - **macvtap interfaces**: FD points to `/dev/tapN` where N is the ifindex → resolve via sysfs

```
midclt call vm.query → [{"name": "vyos", "status": {"pid": 7730, "state": "RUNNING"}}]

/proc/7730/fd/45 → /dev/net/tun
/proc/7730/fdinfo/45 → "iff:\tvnet2"      ← tap interface "vnet2" belongs to "vyos"

/proc/7730/fd/50 → /dev/tap14
/sys/class/net/macvtap0/ifindex → "14"     ← macvtap0 belongs to "vyos"
```

**Fallback — `virsh`** (for non-TrueNAS systems):

1. `virsh list --name --state-running` → get VM names
2. `virsh domiflist <vm>` → get interface names per VM

### Data Flow Diagram

```
┌────────────────────┐     ┌──────────────────────┐     ┌──────────────────────┐
│ /proc/1/net/dev    │     │ Docker Engine API     │     │ TrueNAS midclt API   │
│ (host namespace)   │     │ (unix socket)         │     │ (chroot /host)       │
│                    │     │                       │     │                      │
│ iface: rx/tx stats │     │ /containers/json      │     │ vm.query → name, PID │
└────────┬───────────┘     │ /containers/<id>/json │     └──────────┬───────────┘
         │                 │ /networks             │                │
         │                 └──────────┬────────────┘                │
         │                            │                             │
         ▼                            ▼                             ▼
┌────────────────────────────────────────────────────────────────────────────────┐
│                          NetworkCollector.Collect()                           │
│                                                                              │
│  1. Parse /proc/1/net/dev → map[iface]stats                                  │
│  2. Read sysfs operstate + master symlinks → state, bridge membership        │
│  3. Build host ifindex map → map[ifindex]iface                               │
│  4. Docker: containers → veth mapping + networks → bridge mapping            │
│  5. midclt: VMs → QEMU PIDs → fdinfo → vnet/macvtap mapping                 │
│  6. Classify + emit metrics with enriched labels                             │
└────────────────────────────────────────────────────────────────────────────────┘
         │
         ▼
┌────────────────────┐     ┌──────────────────────┐     ┌──────────────────────┐
│ Prometheus scrape   │────▶│ InfluxDB (via        │────▶│ Grafana dashboards   │
│ :9551/metrics       │     │ Telegraf/remote_write)│     │                      │
└────────────────────┘     └──────────────────────┘     └──────────────────────┘
```

---

## Container Requirements

Running inside Docker requires specific privileges to access host data:

| Requirement | Why |
|---|---|
| `pid: host` | Access `/proc/<PID>/root/sys` for container sysfs, `/proc/<PID>/fdinfo` for QEMU FDs |
| `privileged: true` | Read `/proc/<PID>/fdinfo/` (requires `CAP_SYS_PTRACE`), `chroot /host` for `midclt` |
| `/:/host:ro,rslave` | Host filesystem access for sysfs, procfs, and `chroot` |
| `/var/run/docker.sock` | Docker Engine API for container/network listing |
| `--path.procfs=/host/proc` | Tell exporter where host procfs is mounted |
| `--path.rootfs=/host` | Tell exporter where host rootfs is (for `chroot` commands) |
| `--docker.socket=/host/var/run/docker.sock` | Docker socket inside container |

---

## Running on TrueNAS SCALE

### Docker Compose (Custom App)

```yaml
services:
  truenas-net-exporter:
    image: ghcr.io/lfventura/prometheus-truenas-net-exporter:latest
    container_name: truenas_net_exporter
    pid: host
    privileged: true
    ports:
      - "9551:9551"
    restart: unless-stopped
    command:
      - "--path.rootfs=/host"
      - "--path.procfs=/host/proc"
      - "--docker.socket=/host/var/run/docker.sock"
      - "--web.listen-address=:9551"
    volumes:
      - /:/host:ro,rslave
      - /dev:/host/dev:ro
      - /var/run/docker.sock:/host/var/run/docker.sock:ro
```

### CLI Flags

| Flag | Default | Description |
|---|---|---|
| `--web.listen-address` | `:9551` | Address to listen on |
| `--web.telemetry-path` | `/metrics` | Metrics endpoint path |
| `--path.rootfs` | `/` | Host root filesystem (`/host` in containers) |
| `--path.procfs` | `/proc` | procfs mount point (`/host/proc` in containers) |
| `--docker.socket` | `/var/run/docker.sock` | Docker socket path (`/host/var/run/docker.sock` in containers) |
| `--log.level` | `info` | Log level: `debug`, `info`, `warn`, `error` |
| `--version` | | Print version and exit |

### Prometheus Configuration

```yaml
scrape_configs:
  - job_name: "truenas-net"
    static_configs:
      - targets: ["truenas-host:9551"]
```

---

## Grafana Queries

### PromQL

```promql
# Traffic rate per Docker app
sum by (app) (rate(net_interface_tx_bytes_total{instance_type="docker", app!=""}[5m]))

# Traffic rate per VM
sum by (instance) (rate(net_interface_tx_bytes_total{instance_type="vm"}[5m]))

# Traffic rate per physical NIC
rate(net_interface_tx_bytes_total{instance_type="physical"}[5m])

# Total bridge traffic
sum by (instance) (rate(net_interface_tx_bytes_total{instance_type="bridge", app!=""}[5m]))

# Errors + drops per interface
rate(net_interface_rx_errors_total[5m]) + rate(net_interface_rx_dropped_total[5m])
```

### Flux (InfluxDB)

```flux
// TX throughput per Docker app
from(bucket: "metrics_prometheus")
  |> range(start: v.timeRangeStart, stop: v.timeRangeStop)
  |> filter(fn: (r) => r["_measurement"] == "truenas_netexporter")
  |> filter(fn: (r) => r["_field"] == "net_interface_tx_bytes_total")
  |> filter(fn: (r) => r["instance_type"] == "docker")
  |> filter(fn: (r) => r["app"] != "")
  |> derivative(unit: 1s, nonNegative: true)
  |> aggregateWindow(every: v.windowPeriod, fn: mean, createEmpty: false)
  |> group(columns: ["app"])
  |> sum()

// RX throughput per VM
from(bucket: "metrics_prometheus")
  |> range(start: v.timeRangeStart, stop: v.timeRangeStop)
  |> filter(fn: (r) => r["_measurement"] == "truenas_netexporter")
  |> filter(fn: (r) => r["_field"] == "net_interface_rx_bytes_total")
  |> filter(fn: (r) => r["instance_type"] == "vm")
  |> derivative(unit: 1s, nonNegative: true)
  |> aggregateWindow(every: v.windowPeriod, fn: mean, createEmpty: false)
```

---

## Troubleshooting

### No network metrics (only Go default metrics)

**Symptom**: The `/metrics` endpoint shows `go_*`, `process_*`, and `promhttp_*` metrics but no `net_interface_*`.

**Cause**: The exporter is reading `/proc/net/dev` which is a symlink to `/proc/self/net/dev` → the container's network namespace (only `lo` + `eth0`).

**Fix**: Ensure the exporter reads `/proc/1/net/dev`. Set `--path.procfs=/host/proc` and mount `/:/host:ro,rslave`.

### Docker containers not mapped (veth with no app/instance)

**Symptom**: `instance_type="docker"` but `instance` shows the raw veth name and `app=""`.

**Causes**:
1. Docker socket not accessible → check `--docker.socket` path and volume mount
2. Container PID namespace not shared → ensure `pid: host` in docker-compose
3. Container's sysfs not readable → need `privileged: true` or at minimum `CAP_SYS_PTRACE`

**Debug**: Run with `--log.level=debug` and check for `docker socket not available` or `cannot read container sysfs` messages.

### VMs not mapped (vnet shows interface name instead of VM name)

**Symptom**: `instance_type="vm"` but `instance="vnet0"` instead of `instance="pihole"`.

**Causes**:
1. On TrueNAS: `midclt` not available via chroot → check that `/host` mount includes `/usr/bin/midclt`
2. On other systems: `virsh` not installed or libvirt socket missing
3. QEMU process fdinfo not readable → need `privileged: true`

**Debug**: Run with `--log.level=debug` and check for `vm mapping not available` messages. Verify manually:
```bash
# Inside the exporter container:
chroot /host midclt call vm.query
```

### Bridges show hash names instead of network names

**Symptom**: `instance="br-2c852816592c"` instead of `instance="ix-grafana_default"`.

**Cause**: Docker Networks API not accessible or returned an error.

**Debug**: Verify Docker socket is mounted and the exporter can reach it:
```bash
curl --unix-socket /var/run/docker.sock http://localhost/networks
```

---

## Building

```bash
# Local build
go build -o truenas-net-exporter .

# Docker build
docker compose build

# Run locally (not in container — reads host procfs/sysfs directly)
./truenas-net-exporter --docker.socket=/var/run/docker.sock

# Run in Docker
docker compose up -d
```

## Project Structure

```
main.go                    HTTP server, CLI flags, logger (port 9551)
collector/
  options.go               Options struct (ProcPath, RootfsPath, IsContainer)
  network.go               NetworkCollector: /proc/1/net/dev parsing, interface
                           classification, sysfs reading, bridge/VM/Docker mapping
  docker.go                Docker Engine API client (unix socket HTTP):
                           ListContainers, ListNetworks, inspectContainer
Dockerfile                 Multi-stage: golang:1.26-bookworm → debian:bookworm-slim
docker-compose.yml         Production deployment with required privileges
.github/workflows/
  release.yml              CI/CD: auto-semver from conventional commits, GHCR publish
Makefile                   Build/push/clean shortcuts
```

## License

MIT
