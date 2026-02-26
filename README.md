# TrueNAS Network Exporter for Prometheus

A Prometheus exporter that collects per-network-interface traffic metrics, enriched with Docker container, VM, and application labels. Designed for TrueNAS SCALE but works on any Linux host with Docker.

## Features

- Reads `/proc/net/dev` for all interface traffic counters
- Maps Docker container veth interfaces to container names via Docker API
- Maps VM vnet/macvtap interfaces to VM names via `virsh`
- Extracts application names from Docker Compose project labels
- Resolves Docker bridge names to human-readable network names
- Classifies interfaces: physical, bridge, docker, vm, vlan, macvtap, loopback

## Metrics Exported

### Network Interface Traffic (`/proc/net/dev`)

| Metric | Type | Description |
|---|---|---|
| `net_interface_rx_bytes_total` | Counter | Total bytes received |
| `net_interface_tx_bytes_total` | Counter | Total bytes transmitted |
| `net_interface_rx_packets_total` | Counter | Total packets received |
| `net_interface_tx_packets_total` | Counter | Total packets transmitted |
| `net_interface_rx_errors_total` | Counter | Total receive errors |
| `net_interface_tx_errors_total` | Counter | Total transmit errors |
| `net_interface_rx_dropped_total` | Counter | Total received packets dropped |
| `net_interface_tx_dropped_total` | Counter | Total transmitted packets dropped |

### Labels

| Label | Description |
|---|---|
| `interface` | Host-side interface name (e.g. `veth105cce14`, `enp8s0f0`, `br0`) |
| `instance` | Resolved name: container name for Docker, VM name for VMs, interface name for physical/bridge |
| `instance_type` | `physical`, `bridge`, `docker`, `vm`, `vlan`, `macvtap`, `loopback`, `unknown` |
| `app` | Application name from Docker Compose project (e.g. `grafana`, `immich`, `jellyfin`) |
| `bridge` | Parent bridge name (if interface is a bridge member) |
| `state` | Interface state: `up`, `down`, `unknown` |

### Example Output

```
net_interface_rx_bytes_total{interface="veth105cce14",instance="ix-influxdb-influxdb-1",instance_type="docker",app="influxdb",bridge="br0",state="up"} 8554319
net_interface_tx_bytes_total{interface="vnet0",instance="ubuntu-vm",instance_type="vm",app="",bridge="br0",state="unknown"} 1372652028246
net_interface_rx_bytes_total{interface="enp8s0f0",instance="enp8s0f0",instance_type="physical",app="",bridge="",state="up"} 1110736577306
```

## How It Works

### Docker Container Mapping

1. Connects to Docker socket and lists running containers
2. For each container, reads `/proc/<PID>/root/sys/class/net/eth0/iflink` to get the host-side ifindex
3. Matches ifindex to host interface names via `/sys/class/net/<iface>/ifindex`
4. Extracts app name from `com.docker.compose.project` label (strips `ix-` prefix for TrueNAS)

### VM Mapping

1. Runs `virsh list --name --state-running` to get VM names
2. For each VM, runs `virsh domiflist <vm>` to get interface names (vnet*, macvtap*)
3. Maps interface names to VM names

### Bridge Resolution

- Docker bridges (`br-<hash>`) are resolved to Docker network names by matching NetworkID prefixes
- System bridges (`br0`, `br1`) keep their names
- Bridge membership is detected via sysfs `master` symlinks

## Running on TrueNAS

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

### Flags

| Flag | Default | Description |
|---|---|---|
| `--web.listen-address` | `:9551` | Address to listen on |
| `--web.telemetry-path` | `/metrics` | Metrics endpoint path |
| `--path.rootfs` | `/` | Host root filesystem (use `/host` in containers) |
| `--path.procfs` | `/proc` | procfs mount point (use `/host/proc` in containers) |
| `--docker.socket` | `/var/run/docker.sock` | Docker socket path (use `/host/var/run/docker.sock` in containers) |
| `--log.level` | `info` | Log level: debug, info, warn, error |
| `--version` | | Print version and exit |

### Prometheus Configuration

```yaml
scrape_configs:
  - job_name: "truenas-net"
    static_configs:
      - targets: ["truenas-host:9551"]
```

## Grafana Queries

### Traffic per App (PromQL)

```promql
sum by (app) (rate(net_interface_tx_bytes_total{instance_type="docker"}[5m]))
```

### Traffic per VM (PromQL)

```promql
sum by (instance) (rate(net_interface_tx_bytes_total{instance_type="vm"}[5m]))
```

### Traffic per Physical Interface (PromQL)

```promql
rate(net_interface_tx_bytes_total{instance_type="physical"}[5m])
```

### Traffic per App (Flux)

```flux
from(bucket: "metrics_prometheus")
  |> range(start: v.timeRangeStart, stop: v.timeRangeStop)
  |> filter(fn: (r) => r["_measurement"] == "truenas_netexporter")
  |> filter(fn: (r) => r["_field"] == "net_interface_tx_bytes_total")
  |> filter(fn: (r) => r["instance_type"] == "docker")
  |> derivative(unit: 1s, nonNegative: true)
  |> aggregateWindow(every: v.windowPeriod, fn: mean, createEmpty: false)
  |> group(columns: ["app"])
  |> sum()
```

## Building

```bash
# Local build
go build -o truenas-net-exporter .

# Docker
docker compose build
docker compose up -d
```

## Architecture

```
main.go                    HTTP server + CLI flags (port 9551)
collector/
  options.go               Shared configuration (ProcPath, RootfsPath)
  network.go               Network interface metrics from /proc/net/dev
  docker.go                Minimal Docker API client for container mapping
```

## License

MIT
