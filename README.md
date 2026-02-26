# TrueNAS Network Exporter for Prometheus

A Prometheus exporter that collects per-network-interface traffic metrics on Linux, enriched with Docker container, VM, and application labels. Purpose-built for **TrueNAS SCALE** but works on any Linux host with Docker and/or QEMU VMs.

## Why this exporter?

On a TrueNAS SCALE system, a single host can have 40+ network interfaces: physical NICs, Linux bridges, Docker container veths, VM taps, VLANs, and macvtap devices. Standard tools like `node_exporter` report raw interface names (`veth402dbe0`, `vnet3`, `br-2c852816592c`) — meaningless without cross-referencing Docker, QEMU, and sysfs. This exporter enriches every interface with:

- **What it is** (`instance_type`): physical, bridge, docker, vm, vlan, macvtap, loopback
- **Who owns it** (`instance`): container name, VM name, or interface name
- **Which application** (`app`): derived from Docker Compose project labels or Docker network names
- **Bridge membership** (`bridge`): which bridge the interface is attached to
- **VLAN ID** (`vlan`): 802.1Q VLAN ID (from `/proc/net/vlan/config`), inherited by bridge members
- **Link state** (`state`): up, down, or unknown

## Features

- Reads traffic counters from `/proc/1/net/dev` (host network namespace)
- Maps Docker container veth interfaces → container names via Docker Engine API
- Maps Incus/LXC container veth interfaces → container names via cgroup scanning
- Maps VM vnet/macvtap interfaces → VM names via TrueNAS `midclt` API (with `virsh` fallback)
- Resolves Docker bridge interfaces → Docker network names via Networks API
- Extracts application names from Docker Compose project labels (`ix-<app>` prefix for TrueNAS apps)
- Derives `app` label for bridges and orphan veths from their Docker network name
- Discovers 802.1Q VLANs from `/proc/net/vlan/config` and propagates VLAN IDs to bridges and their members
- Classifies all interfaces: `physical`, `bridge`, `docker`, `incus`, `vm`, `vlan`, `macvtap`, `loopback`

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
| `instance_type` | Interface classification | `physical`, `bridge`, `docker`, `incus`, `vm`, `vlan`, `macvtap`, `loopback` |
| `app` | Application name (from Docker Compose project or network) | `grafana`, `immich`, `jellyfin` |
| `bridge` | Parent bridge (if interface is a bridge member) | `br0`, `br-2c852816592c` |
| `vlan` | 802.1Q VLAN ID (inherited from bridge uplink) | `1`, `100`, `200` |
| `state` | Link state from sysfs operstate | `up`, `down`, `unknown` |

### Example Output

```
# Physical NIC
net_interface_rx_bytes_total{interface="enp8s0f0",instance="enp8s0f0",instance_type="physical",app="system",bridge="",vlan="",state="up"} 1.111594096922e+12

# Docker container mapped to app (inherits VLAN from bridge)
net_interface_rx_bytes_total{interface="veth402dbe0",instance="ix-grafana-grafana-1",instance_type="docker",app="grafana",bridge="br-2c852816592c",vlan="",state="up"} 2.56302961e+08

# Docker bridge mapped to network name with app
net_interface_rx_bytes_total{interface="br-2c852816592c",instance="ix-grafana_default",instance_type="bridge",app="grafana",bridge="",vlan="",state="up"} 2.54524065e+08

# VM tap interface mapped to VM name (inherits VLAN 1 from br0)
net_interface_rx_bytes_total{interface="vnet3",instance="vyos",instance_type="vm",app="vyos",bridge="br0",vlan="1",state="unknown"} 1.115796347231e+12

# macvtap interface mapped to VM name
net_interface_rx_bytes_total{interface="macvtap0",instance="vyos",instance_type="macvtap",app="vyos",bridge="",vlan="",state="up"} 1.111594084954e+12

# Incus/LXC container (inherits VLAN 1 from br0)
net_interface_rx_bytes_total{interface="veth105cce14",instance="backupserver",instance_type="incus",app="backupserver",bridge="br0",vlan="1",state="up"} 8.559759e+06

# System bridge (VLAN 1 because vlan1 is a member)
net_interface_rx_bytes_total{interface="br0",instance="br0",instance_type="bridge",app="system",bridge="",vlan="1",state="up"} 1.343738956933e+12

# VLAN sub-interface
net_interface_rx_bytes_total{interface="vlan1",instance="vlan1",instance_type="vlan",app="system",bridge="br0",vlan="1",state="up"} 2.17320405154e+11
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
  Incus/LXC Containers  │  veth105cce14 ── backupserver (br0)     │
                        │  vethXXXXXXXX ── crapscrap (br0)        │
                        │  vethXXXXXXXX ── pzserver (br0)         │
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

### Step 6: Incus/LXC Container Mapping (veth → container name)

Incus/LXC containers use veth pairs like Docker but are not managed by the Docker API. They're discovered by scanning `/proc` for processes in LXC cgroups.

**Mapping process**:

1. Scan `/proc/<PID>/cgroup` for all processes
2. Look for cgroup paths matching `lxc.payload.<containername>/init.scope`
3. Only match `/init.scope` to find the container's init process (avoids processing every process inside the container)
4. Use the same iflink technique as Docker to map the container's veth to the host

```
/proc/8444/cgroup → "0::/lxc.payload.backupserver/init.scope"
  └── Container name: "backupserver", Init PID: 8444

/proc/8444/root/sys/class/net/eth0/iflink → "18"
/sys/class/net/veth105cce14/ifindex → "18"
  ∴ veth105cce14 belongs to Incus container "backupserver"
```

Incus containers are labeled with `instance_type="incus"` to distinguish them from Docker containers.

### Step 7: VLAN Detection (`/proc/net/vlan/config`)

802.1Q VLAN sub-interfaces are discovered by parsing `/proc/1/net/vlan/config`:

```
VLAN Dev name       | VLAN ID
Name-Type: VLAN_NAME_TYPE_RAW_PLUS_VID_NO_PAD
vlan1              | 1  | enp8s0f1
vlan2              | 2  | enp8s0f0
```

**VLAN propagation**:

1. VLAN sub-interfaces (e.g., `vlan1`) get the VLAN ID directly (`vlan="1"`)
2. Bridges inherit the VLAN ID from any VLAN member: if `vlan1` is a member of `br0`, then `br0` gets `vlan="1"`
3. All bridge members (veths, vnets) inherit the VLAN from their parent bridge
4. Non-VLAN dot-notation interfaces (e.g., `eno1.100`) are also detected and reclassified as `instance_type="vlan"`

This allows filtering and grouping by VLAN across all interface types:
```
# Which VMs are on VLAN 1?
net_interface_rx_bytes_total{instance_type="vm", vlan="1"}

# Traffic per VLAN
sum by (vlan) (rate(net_interface_tx_bytes_total{vlan!=""}[5m]))
```

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
│  6. /proc/1/net/vlan/config → VLAN IDs, propagated to bridges + members      │
│  7. Classify + emit metrics with enriched labels                             │
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

### PromQL (Prometheus)

```promql
# Traffic rate per Docker app
sum by (app) (rate(net_interface_tx_bytes_total{instance_type="docker", app!=""}[5m]))

# Traffic rate per VM
sum by (instance) (rate(net_interface_tx_bytes_total{instance_type="vm"}[5m]))

# Traffic rate per Incus/LXC container
sum by (instance) (rate(net_interface_tx_bytes_total{instance_type="incus"}[5m]))

# Traffic rate per physical NIC
rate(net_interface_tx_bytes_total{instance_type="physical"}[5m])

# Total bridge traffic
sum by (instance) (rate(net_interface_tx_bytes_total{instance_type="bridge", app!=""}[5m]))

# Traffic per VLAN
sum by (vlan) (rate(net_interface_tx_bytes_total{vlan!=""}[5m]))

# Errors + drops per interface
rate(net_interface_rx_errors_total[5m]) + rate(net_interface_rx_dropped_total[5m])

# All VMs on VLAN 1
rate(net_interface_tx_bytes_total{instance_type="vm", vlan="1"}[5m])
```

### Flux (InfluxDB)

All Flux examples below use bucket `metrics_prometheus`. Replace with your bucket name. The `_measurement` value depends on your Telegraf/scrape configuration.

### 1. Overview Dashboard

```flux
// Total RX throughput — all interfaces combined (bytes/s)
from(bucket: "metrics_prometheus")
  |> range(start: v.timeRangeStart, stop: v.timeRangeStop)
  |> filter(fn: (r) => r["_measurement"] == "truenas_netexporter")
  |> filter(fn: (r) => r["_field"] == "net_interface_rx_bytes_total")
  |> derivative(unit: 1s, nonNegative: true)
  |> aggregateWindow(every: v.windowPeriod, fn: mean, createEmpty: false)
  |> group()
  |> sum()

// Total TX throughput — all interfaces combined (bytes/s)
from(bucket: "metrics_prometheus")
  |> range(start: v.timeRangeStart, stop: v.timeRangeStop)
  |> filter(fn: (r) => r["_measurement"] == "truenas_netexporter")
  |> filter(fn: (r) => r["_field"] == "net_interface_tx_bytes_total")
  |> derivative(unit: 1s, nonNegative: true)
  |> aggregateWindow(every: v.windowPeriod, fn: mean, createEmpty: false)
  |> group()
  |> sum()

// Traffic breakdown by instance_type (stacked area)
from(bucket: "metrics_prometheus")
  |> range(start: v.timeRangeStart, stop: v.timeRangeStop)
  |> filter(fn: (r) => r["_measurement"] == "truenas_netexporter")
  |> filter(fn: (r) => r["_field"] == "net_interface_tx_bytes_total")
  |> derivative(unit: 1s, nonNegative: true)
  |> aggregateWindow(every: v.windowPeriod, fn: mean, createEmpty: false)
  |> group(columns: ["instance_type"])
  |> sum()

// Total traffic by VLAN (stacked area)
from(bucket: "metrics_prometheus")
  |> range(start: v.timeRangeStart, stop: v.timeRangeStop)
  |> filter(fn: (r) => r["_measurement"] == "truenas_netexporter")
  |> filter(fn: (r) => r["_field"] == "net_interface_tx_bytes_total")
  |> filter(fn: (r) => r["vlan"] != "")
  |> derivative(unit: 1s, nonNegative: true)
  |> aggregateWindow(every: v.windowPeriod, fn: mean, createEmpty: false)
  |> group(columns: ["vlan"])
  |> sum()

// Interface count by type (stat panel)
from(bucket: "metrics_prometheus")
  |> range(start: -5m)
  |> filter(fn: (r) => r["_measurement"] == "truenas_netexporter")
  |> filter(fn: (r) => r["_field"] == "net_interface_rx_bytes_total")
  |> last()
  |> group(columns: ["instance_type"])
  |> count()
```

### 2. Per-Application Dashboard

```flux
// TX throughput per app (stacked area — top talkers)
from(bucket: "metrics_prometheus")
  |> range(start: v.timeRangeStart, stop: v.timeRangeStop)
  |> filter(fn: (r) => r["_measurement"] == "truenas_netexporter")
  |> filter(fn: (r) => r["_field"] == "net_interface_tx_bytes_total")
  |> filter(fn: (r) => r["app"] != "" and r["app"] != "system")
  |> derivative(unit: 1s, nonNegative: true)
  |> aggregateWindow(every: v.windowPeriod, fn: mean, createEmpty: false)
  |> group(columns: ["app"])
  |> sum()

// RX throughput per app
from(bucket: "metrics_prometheus")
  |> range(start: v.timeRangeStart, stop: v.timeRangeStop)
  |> filter(fn: (r) => r["_measurement"] == "truenas_netexporter")
  |> filter(fn: (r) => r["_field"] == "net_interface_rx_bytes_total")
  |> filter(fn: (r) => r["app"] != "" and r["app"] != "system")
  |> derivative(unit: 1s, nonNegative: true)
  |> aggregateWindow(every: v.windowPeriod, fn: mean, createEmpty: false)
  |> group(columns: ["app"])
  |> sum()

// Total bytes transferred per app (bar chart — last 24h)
from(bucket: "metrics_prometheus")
  |> range(start: -24h)
  |> filter(fn: (r) => r["_measurement"] == "truenas_netexporter")
  |> filter(fn: (r) => r["_field"] == "net_interface_tx_bytes_total")
  |> filter(fn: (r) => r["app"] != "" and r["app"] != "system")
  |> derivative(unit: 1s, nonNegative: true)
  |> group(columns: ["app"])
  |> sum()
  |> group()
  |> sort(columns: ["_value"], desc: true)
```

### 3. Docker Containers Dashboard

```flux
// TX throughput per Docker container
from(bucket: "metrics_prometheus")
  |> range(start: v.timeRangeStart, stop: v.timeRangeStop)
  |> filter(fn: (r) => r["_measurement"] == "truenas_netexporter")
  |> filter(fn: (r) => r["_field"] == "net_interface_tx_bytes_total")
  |> filter(fn: (r) => r["instance_type"] == "docker")
  |> derivative(unit: 1s, nonNegative: true)
  |> aggregateWindow(every: v.windowPeriod, fn: mean, createEmpty: false)

// Per-container packets/s
from(bucket: "metrics_prometheus")
  |> range(start: v.timeRangeStart, stop: v.timeRangeStop)
  |> filter(fn: (r) => r["_measurement"] == "truenas_netexporter")
  |> filter(fn: (r) => r["_field"] == "net_interface_tx_packets_total" or r["_field"] == "net_interface_rx_packets_total")
  |> filter(fn: (r) => r["instance_type"] == "docker")
  |> derivative(unit: 1s, nonNegative: true)
  |> aggregateWindow(every: v.windowPeriod, fn: mean, createEmpty: false)

// Docker containers grouped by app with RX+TX combined
import "join"

rx = from(bucket: "metrics_prometheus")
  |> range(start: v.timeRangeStart, stop: v.timeRangeStop)
  |> filter(fn: (r) => r["_measurement"] == "truenas_netexporter")
  |> filter(fn: (r) => r["_field"] == "net_interface_rx_bytes_total")
  |> filter(fn: (r) => r["instance_type"] == "docker")
  |> derivative(unit: 1s, nonNegative: true)
  |> aggregateWindow(every: v.windowPeriod, fn: mean, createEmpty: false)
  |> group(columns: ["app"])
  |> sum()
  |> set(key: "direction", value: "rx")

tx = from(bucket: "metrics_prometheus")
  |> range(start: v.timeRangeStart, stop: v.timeRangeStop)
  |> filter(fn: (r) => r["_measurement"] == "truenas_netexporter")
  |> filter(fn: (r) => r["_field"] == "net_interface_tx_bytes_total")
  |> filter(fn: (r) => r["instance_type"] == "docker")
  |> derivative(unit: 1s, nonNegative: true)
  |> aggregateWindow(every: v.windowPeriod, fn: mean, createEmpty: false)
  |> group(columns: ["app"])
  |> sum()
  |> set(key: "direction", value: "tx")

union(tables: [rx, tx])
```

### 4. Virtual Machines Dashboard

```flux
// TX throughput per VM
from(bucket: "metrics_prometheus")
  |> range(start: v.timeRangeStart, stop: v.timeRangeStop)
  |> filter(fn: (r) => r["_measurement"] == "truenas_netexporter")
  |> filter(fn: (r) => r["_field"] == "net_interface_tx_bytes_total")
  |> filter(fn: (r) => r["instance_type"] == "vm" or r["instance_type"] == "macvtap")
  |> derivative(unit: 1s, nonNegative: true)
  |> aggregateWindow(every: v.windowPeriod, fn: mean, createEmpty: false)
  |> group(columns: ["instance"])
  |> sum()

// RX throughput per VM
from(bucket: "metrics_prometheus")
  |> range(start: v.timeRangeStart, stop: v.timeRangeStop)
  |> filter(fn: (r) => r["_measurement"] == "truenas_netexporter")
  |> filter(fn: (r) => r["_field"] == "net_interface_rx_bytes_total")
  |> filter(fn: (r) => r["instance_type"] == "vm" or r["instance_type"] == "macvtap")
  |> derivative(unit: 1s, nonNegative: true)
  |> aggregateWindow(every: v.windowPeriod, fn: mean, createEmpty: false)
  |> group(columns: ["instance"])
  |> sum()

// VM packets/s per VM
from(bucket: "metrics_prometheus")
  |> range(start: v.timeRangeStart, stop: v.timeRangeStop)
  |> filter(fn: (r) => r["_measurement"] == "truenas_netexporter")
  |> filter(fn: (r) => r["_field"] == "net_interface_tx_packets_total")
  |> filter(fn: (r) => r["instance_type"] == "vm" or r["instance_type"] == "macvtap")
  |> derivative(unit: 1s, nonNegative: true)
  |> aggregateWindow(every: v.windowPeriod, fn: mean, createEmpty: false)
  |> group(columns: ["instance"])
  |> sum()

// VMs by VLAN (table panel)
from(bucket: "metrics_prometheus")
  |> range(start: -5m)
  |> filter(fn: (r) => r["_measurement"] == "truenas_netexporter")
  |> filter(fn: (r) => r["_field"] == "net_interface_rx_bytes_total")
  |> filter(fn: (r) => r["instance_type"] == "vm")
  |> last()
  |> keep(columns: ["instance", "interface", "bridge", "vlan", "state"])
```

### 5. Incus/LXC Containers Dashboard

```flux
// TX throughput per Incus container
from(bucket: "metrics_prometheus")
  |> range(start: v.timeRangeStart, stop: v.timeRangeStop)
  |> filter(fn: (r) => r["_measurement"] == "truenas_netexporter")
  |> filter(fn: (r) => r["_field"] == "net_interface_tx_bytes_total")
  |> filter(fn: (r) => r["instance_type"] == "incus")
  |> derivative(unit: 1s, nonNegative: true)
  |> aggregateWindow(every: v.windowPeriod, fn: mean, createEmpty: false)

// RX throughput per Incus container
from(bucket: "metrics_prometheus")
  |> range(start: v.timeRangeStart, stop: v.timeRangeStop)
  |> filter(fn: (r) => r["_measurement"] == "truenas_netexporter")
  |> filter(fn: (r) => r["_field"] == "net_interface_rx_bytes_total")
  |> filter(fn: (r) => r["instance_type"] == "incus")
  |> derivative(unit: 1s, nonNegative: true)
  |> aggregateWindow(every: v.windowPeriod, fn: mean, createEmpty: false)
```

### 6. Bridge & VLAN Dashboard

```flux
// TX throughput per bridge
from(bucket: "metrics_prometheus")
  |> range(start: v.timeRangeStart, stop: v.timeRangeStop)
  |> filter(fn: (r) => r["_measurement"] == "truenas_netexporter")
  |> filter(fn: (r) => r["_field"] == "net_interface_tx_bytes_total")
  |> filter(fn: (r) => r["instance_type"] == "bridge")
  |> derivative(unit: 1s, nonNegative: true)
  |> aggregateWindow(every: v.windowPeriod, fn: mean, createEmpty: false)

// Traffic per VLAN (sum all members)
from(bucket: "metrics_prometheus")
  |> range(start: v.timeRangeStart, stop: v.timeRangeStop)
  |> filter(fn: (r) => r["_measurement"] == "truenas_netexporter")
  |> filter(fn: (r) => r["_field"] == "net_interface_tx_bytes_total")
  |> filter(fn: (r) => r["vlan"] != "")
  |> derivative(unit: 1s, nonNegative: true)
  |> aggregateWindow(every: v.windowPeriod, fn: mean, createEmpty: false)
  |> group(columns: ["vlan"])
  |> sum()

// Bridge member count (stat panel)
from(bucket: "metrics_prometheus")
  |> range(start: -5m)
  |> filter(fn: (r) => r["_measurement"] == "truenas_netexporter")
  |> filter(fn: (r) => r["_field"] == "net_interface_rx_bytes_total")
  |> filter(fn: (r) => r["bridge"] != "")
  |> last()
  |> group(columns: ["bridge"])
  |> count()

// All interfaces on a specific VLAN
from(bucket: "metrics_prometheus")
  |> range(start: v.timeRangeStart, stop: v.timeRangeStop)
  |> filter(fn: (r) => r["_measurement"] == "truenas_netexporter")
  |> filter(fn: (r) => r["_field"] == "net_interface_tx_bytes_total")
  |> filter(fn: (r) => r["vlan"] == "1")  // change VLAN ID as needed
  |> derivative(unit: 1s, nonNegative: true)
  |> aggregateWindow(every: v.windowPeriod, fn: mean, createEmpty: false)
```

### 7. Physical NICs Dashboard

```flux
// TX throughput per physical NIC
from(bucket: "metrics_prometheus")
  |> range(start: v.timeRangeStart, stop: v.timeRangeStop)
  |> filter(fn: (r) => r["_measurement"] == "truenas_netexporter")
  |> filter(fn: (r) => r["_field"] == "net_interface_tx_bytes_total")
  |> filter(fn: (r) => r["instance_type"] == "physical")
  |> derivative(unit: 1s, nonNegative: true)
  |> aggregateWindow(every: v.windowPeriod, fn: mean, createEmpty: false)

// RX throughput per physical NIC
from(bucket: "metrics_prometheus")
  |> range(start: v.timeRangeStart, stop: v.timeRangeStop)
  |> filter(fn: (r) => r["_measurement"] == "truenas_netexporter")
  |> filter(fn: (r) => r["_field"] == "net_interface_rx_bytes_total")
  |> filter(fn: (r) => r["instance_type"] == "physical")
  |> derivative(unit: 1s, nonNegative: true)
  |> aggregateWindow(every: v.windowPeriod, fn: mean, createEmpty: false)

// Physical NIC utilization % (assuming 10Gbps = 1250000000 bytes/s)
from(bucket: "metrics_prometheus")
  |> range(start: v.timeRangeStart, stop: v.timeRangeStop)
  |> filter(fn: (r) => r["_measurement"] == "truenas_netexporter")
  |> filter(fn: (r) => r["_field"] == "net_interface_tx_bytes_total")
  |> filter(fn: (r) => r["instance_type"] == "physical")
  |> derivative(unit: 1s, nonNegative: true)
  |> map(fn: (r) => ({r with _value: r._value / 1250000000.0 * 100.0}))
  |> aggregateWindow(every: v.windowPeriod, fn: mean, createEmpty: false)
```

### 8. Errors & Drops Dashboard

```flux
// RX errors per interface
from(bucket: "metrics_prometheus")
  |> range(start: v.timeRangeStart, stop: v.timeRangeStop)
  |> filter(fn: (r) => r["_measurement"] == "truenas_netexporter")
  |> filter(fn: (r) => r["_field"] == "net_interface_rx_errors_total")
  |> derivative(unit: 1s, nonNegative: true)
  |> filter(fn: (r) => r._value > 0)
  |> aggregateWindow(every: v.windowPeriod, fn: mean, createEmpty: false)

// TX errors per interface
from(bucket: "metrics_prometheus")
  |> range(start: v.timeRangeStart, stop: v.timeRangeStop)
  |> filter(fn: (r) => r["_measurement"] == "truenas_netexporter")
  |> filter(fn: (r) => r["_field"] == "net_interface_tx_errors_total")
  |> derivative(unit: 1s, nonNegative: true)
  |> filter(fn: (r) => r._value > 0)
  |> aggregateWindow(every: v.windowPeriod, fn: mean, createEmpty: false)

// RX drops per interface
from(bucket: "metrics_prometheus")
  |> range(start: v.timeRangeStart, stop: v.timeRangeStop)
  |> filter(fn: (r) => r["_measurement"] == "truenas_netexporter")
  |> filter(fn: (r) => r["_field"] == "net_interface_rx_dropped_total")
  |> derivative(unit: 1s, nonNegative: true)
  |> filter(fn: (r) => r._value > 0)
  |> aggregateWindow(every: v.windowPeriod, fn: mean, createEmpty: false)

// TX drops per interface
from(bucket: "metrics_prometheus")
  |> range(start: v.timeRangeStart, stop: v.timeRangeStop)
  |> filter(fn: (r) => r["_measurement"] == "truenas_netexporter")
  |> filter(fn: (r) => r["_field"] == "net_interface_tx_dropped_total")
  |> derivative(unit: 1s, nonNegative: true)
  |> filter(fn: (r) => r._value > 0)
  |> aggregateWindow(every: v.windowPeriod, fn: mean, createEmpty: false)

// Combined errors+drops per app (alert-worthy)
from(bucket: "metrics_prometheus")
  |> range(start: v.timeRangeStart, stop: v.timeRangeStop)
  |> filter(fn: (r) => r["_measurement"] == "truenas_netexporter")
  |> filter(fn: (r) => r["_field"] == "net_interface_rx_errors_total"
                     or r["_field"] == "net_interface_tx_errors_total"
                     or r["_field"] == "net_interface_rx_dropped_total"
                     or r["_field"] == "net_interface_tx_dropped_total")
  |> derivative(unit: 1s, nonNegative: true)
  |> filter(fn: (r) => r._value > 0)
  |> aggregateWindow(every: v.windowPeriod, fn: mean, createEmpty: false)
  |> group(columns: ["app"])
  |> sum()
```

### 9. Master Inventory Table

```flux
// Full interface inventory (table panel)
from(bucket: "metrics_prometheus")
  |> range(start: -5m)
  |> filter(fn: (r) => r["_measurement"] == "truenas_netexporter")
  |> filter(fn: (r) => r["_field"] == "net_interface_rx_bytes_total")
  |> last()
  |> keep(columns: ["interface", "instance", "instance_type", "app", "bridge", "vlan", "state", "_value"])
  |> rename(columns: {_value: "rx_bytes_total"})
  |> sort(columns: ["instance_type", "app", "instance"])
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

### Incus/LXC containers not mapped

**Symptom**: `instance_type="docker"` for an Incus container, `instance` shows raw veth name.

**Cause**: The exporter scans `/proc/<PID>/cgroup` for `lxc.payload.<name>/init.scope`. If the cgroup structure differs (older LXC versions), the pattern won't match.

**Debug**: Check the cgroup format of your Incus container:
```bash
cat /proc/<container-init-pid>/cgroup
# Expected: 0::/lxc.payload.containername/init.scope
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
