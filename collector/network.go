package collector

import (
	"bufio"
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// NetworkCollector collects per-network-interface traffic metrics and
// enriches them with instance/application labels by correlating with
// Docker containers and bridge membership information.
type NetworkCollector struct {
	rxBytes   *prometheus.Desc
	txBytes   *prometheus.Desc
	rxPackets *prometheus.Desc
	txPackets *prometheus.Desc
	rxErrors  *prometheus.Desc
	txErrors  *prometheus.Desc
	rxDropped *prometheus.Desc
	txDropped *prometheus.Desc

	opts         Options
	dockerSocket string
	logger       *slog.Logger
}

// interfaceInfo contains resolved metadata for one network interface.
type interfaceInfo struct {
	Name         string
	Instance     string // resolved name (container name, VM name, or iface name)
	InstanceType string // "physical", "bridge", "docker", "vm", "vlan", "macvtap", "loopback", "unknown"
	App          string // application name (Docker Compose project)
	Bridge       string // parent bridge, if any
	State        string // "up", "down", "unknown"
}

// interfaceStats holds counters parsed from /proc/net/dev.
type interfaceStats struct {
	RxBytes   uint64
	RxPackets uint64
	RxErrors  uint64
	RxDropped uint64
	TxBytes   uint64
	TxPackets uint64
	TxErrors  uint64
	TxDropped uint64
}

// NewNetworkCollector returns a collector that exposes per-interface network
// traffic metrics with container/instance enrichment labels.
func NewNetworkCollector(logger *slog.Logger, opts Options, dockerSocket string) *NetworkCollector {
	labels := []string{"interface", "instance", "instance_type", "app", "bridge", "state"}

	return &NetworkCollector{
		opts:         opts,
		dockerSocket: dockerSocket,
		logger:       logger,
		rxBytes: prometheus.NewDesc(
			"net_interface_rx_bytes_total",
			"Total bytes received on this interface.",
			labels, nil,
		),
		txBytes: prometheus.NewDesc(
			"net_interface_tx_bytes_total",
			"Total bytes transmitted on this interface.",
			labels, nil,
		),
		rxPackets: prometheus.NewDesc(
			"net_interface_rx_packets_total",
			"Total packets received on this interface.",
			labels, nil,
		),
		txPackets: prometheus.NewDesc(
			"net_interface_tx_packets_total",
			"Total packets transmitted on this interface.",
			labels, nil,
		),
		rxErrors: prometheus.NewDesc(
			"net_interface_rx_errors_total",
			"Total receive errors on this interface.",
			labels, nil,
		),
		txErrors: prometheus.NewDesc(
			"net_interface_tx_errors_total",
			"Total transmit errors on this interface.",
			labels, nil,
		),
		rxDropped: prometheus.NewDesc(
			"net_interface_rx_dropped_total",
			"Total received packets dropped on this interface.",
			labels, nil,
		),
		txDropped: prometheus.NewDesc(
			"net_interface_tx_dropped_total",
			"Total transmitted packets dropped on this interface.",
			labels, nil,
		),
	}
}

// Describe implements prometheus.Collector.
func (c *NetworkCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.rxBytes
	ch <- c.txBytes
	ch <- c.rxPackets
	ch <- c.txPackets
	ch <- c.rxErrors
	ch <- c.txErrors
	ch <- c.rxDropped
	ch <- c.txDropped
}

// Collect implements prometheus.Collector.
func (c *NetworkCollector) Collect(ch chan<- prometheus.Metric) {
	// 1. Read interface stats from /proc/net/dev.
	stats, err := c.readProcNetDev()
	if err != nil {
		c.logger.Error("failed to read /proc/net/dev", "error", err)
		return
	}

	// 2. Build interface → metadata mapping.
	infoMap := c.buildInterfaceInfo(stats)

	// 3. Emit metrics.
	for iface, s := range stats {
		info, ok := infoMap[iface]
		if !ok {
			continue
		}

		labels := []string{info.Name, info.Instance, info.InstanceType, info.App, info.Bridge, info.State}

		ch <- prometheus.MustNewConstMetric(c.rxBytes, prometheus.CounterValue, float64(s.RxBytes), labels...)
		ch <- prometheus.MustNewConstMetric(c.txBytes, prometheus.CounterValue, float64(s.TxBytes), labels...)
		ch <- prometheus.MustNewConstMetric(c.rxPackets, prometheus.CounterValue, float64(s.RxPackets), labels...)
		ch <- prometheus.MustNewConstMetric(c.txPackets, prometheus.CounterValue, float64(s.TxPackets), labels...)
		ch <- prometheus.MustNewConstMetric(c.rxErrors, prometheus.CounterValue, float64(s.RxErrors), labels...)
		ch <- prometheus.MustNewConstMetric(c.txErrors, prometheus.CounterValue, float64(s.TxErrors), labels...)
		ch <- prometheus.MustNewConstMetric(c.rxDropped, prometheus.CounterValue, float64(s.RxDropped), labels...)
		ch <- prometheus.MustNewConstMetric(c.txDropped, prometheus.CounterValue, float64(s.TxDropped), labels...)
	}
}

// readProcNetDev parses /proc/net/dev and returns counters per interface.
func (c *NetworkCollector) readProcNetDev() (map[string]interfaceStats, error) {
	path := filepath.Join(c.opts.ProcPath, "net", "dev")
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	result := make(map[string]interfaceStats)
	scanner := bufio.NewScanner(f)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		if lineNo <= 2 {
			continue // skip header lines
		}
		line := scanner.Text()
		iface, s, err := parseProcNetDevLine(line)
		if err != nil {
			continue
		}
		result[iface] = s
	}
	return result, scanner.Err()
}

// parseProcNetDevLine parses one line from /proc/net/dev.
// Format:  iface: rx_bytes rx_packets rx_errs rx_drop rx_fifo rx_frame rx_compressed rx_multicast tx_bytes tx_packets tx_errs tx_drop tx_fifo tx_colls tx_carrier tx_compressed
func parseProcNetDevLine(line string) (string, interfaceStats, error) {
	// Split at colon.
	parts := strings.SplitN(line, ":", 2)
	if len(parts) != 2 {
		return "", interfaceStats{}, fmt.Errorf("no colon in line")
	}
	iface := strings.TrimSpace(parts[0])
	fields := strings.Fields(parts[1])
	if len(fields) < 16 {
		return "", interfaceStats{}, fmt.Errorf("not enough fields")
	}

	vals := make([]uint64, 16)
	for i := 0; i < 16; i++ {
		v, err := strconv.ParseUint(fields[i], 10, 64)
		if err != nil {
			return "", interfaceStats{}, err
		}
		vals[i] = v
	}

	return iface, interfaceStats{
		RxBytes:   vals[0],
		RxPackets: vals[1],
		RxErrors:  vals[2],
		RxDropped: vals[3],
		TxBytes:   vals[8],
		TxPackets: vals[9],
		TxErrors:  vals[10],
		TxDropped: vals[11],
	}, nil
}

// buildInterfaceInfo resolves metadata for each interface name.
func (c *NetworkCollector) buildInterfaceInfo(stats map[string]interfaceStats) map[string]interfaceInfo {
	sysNetPath := c.sysClassNetPath()

	// Read operstate for each interface.
	stateMap := make(map[string]string)
	for iface := range stats {
		stateMap[iface] = readFileString(filepath.Join(sysNetPath, iface, "operstate"))
	}

	// Build bridge membership map: interface → bridge name.
	bridgeMap := c.buildBridgeMap(stats, sysNetPath)

	// Build ifindex → iface name map for the host.
	ifindexMap := c.buildIfindexMap(stats, sysNetPath)

	// Query Docker for container → veth mapping.
	vethToContainer := c.buildDockerMapping(ifindexMap, sysNetPath)

	// Query virsh for VM → vnet mapping.
	vnetToVM := c.buildVMMapping()

	result := make(map[string]interfaceInfo)
	for iface := range stats {
		info := interfaceInfo{
			Name:   iface,
			State:  normalizeState(stateMap[iface]),
			Bridge: bridgeMap[iface],
		}

		switch {
		case iface == "lo":
			info.InstanceType = "loopback"
			info.Instance = "loopback"

		case strings.HasPrefix(iface, "veth"):
			// Docker container veth.
			if ci, ok := vethToContainer[iface]; ok {
				info.InstanceType = "docker"
				info.Instance = ci.Name
				info.App = AppName(ci)
			} else {
				info.InstanceType = "docker"
				info.Instance = iface
			}

		case strings.HasPrefix(iface, "vnet"):
			// VM network interface (libvirt tap/tun).
			info.InstanceType = "vm"
			if vmName, ok := vnetToVM[iface]; ok {
				info.Instance = vmName
			} else {
				info.Instance = iface
			}

		case strings.HasPrefix(iface, "macvtap") || strings.HasPrefix(iface, "macvlan"):
			info.InstanceType = "macvtap"
			if vmName, ok := vnetToVM[iface]; ok {
				info.Instance = vmName
			} else {
				info.Instance = iface
			}

		case strings.HasPrefix(iface, "vlan"):
			info.InstanceType = "vlan"
			info.Instance = iface

		case strings.HasPrefix(iface, "br-") || strings.HasPrefix(iface, "br") ||
			strings.HasPrefix(iface, "docker") || strings.HasPrefix(iface, "incus"):
			info.InstanceType = "bridge"
			info.Instance = c.resolveBridgeName(iface, vethToContainer)

		default:
			// Check if it's a physical device (has a device/driver symlink in sysfs).
			driverPath := filepath.Join(sysNetPath, iface, "device", "driver")
			if _, err := os.Readlink(driverPath); err == nil {
				info.InstanceType = "physical"
			} else {
				info.InstanceType = "unknown"
			}
			info.Instance = iface
		}

		result[iface] = info
	}

	return result
}

// sysClassNetPath returns the path to /sys/class/net (respecting container paths).
func (c *NetworkCollector) sysClassNetPath() string {
	if c.opts.IsContainer() {
		return filepath.Join(c.opts.RootfsPath, "sys", "class", "net")
	}
	return "/sys/class/net"
}

// buildBridgeMap returns a mapping from interface name → parent bridge name.
func (c *NetworkCollector) buildBridgeMap(stats map[string]interfaceStats, sysNetPath string) map[string]string {
	bridgeMap := make(map[string]string)
	for iface := range stats {
		// In sysfs, bridge membership is indicated by a "master" symlink.
		masterLink := filepath.Join(sysNetPath, iface, "master")
		target, err := os.Readlink(masterLink)
		if err != nil {
			continue
		}
		bridgeName := filepath.Base(target)
		bridgeMap[iface] = bridgeName
	}
	return bridgeMap
}

// buildIfindexMap returns a mapping from ifindex number → interface name.
func (c *NetworkCollector) buildIfindexMap(stats map[string]interfaceStats, sysNetPath string) map[int]string {
	m := make(map[int]string)
	for iface := range stats {
		idxStr := readFileString(filepath.Join(sysNetPath, iface, "ifindex"))
		if idx, err := strconv.Atoi(idxStr); err == nil {
			m[idx] = iface
		}
	}
	return m
}

// buildDockerMapping queries the Docker API and maps host-side veth interfaces
// to their owning containers by correlating sysfs iflink values.
func (c *NetworkCollector) buildDockerMapping(ifindexMap map[int]string, sysNetPath string) map[string]ContainerInfo {
	result := make(map[string]ContainerInfo)

	client := NewDockerClient(c.dockerSocket)
	if !client.Available() {
		c.logger.Debug("docker socket not available, skipping container mapping")
		return result
	}

	containers, err := client.ListContainers()
	if err != nil {
		c.logger.Warn("failed to list docker containers", "error", err)
		return result
	}

	// For each container, find its host-side veth by reading the peer iflink
	// from the container's network namespace via /proc/<PID>/root/sys.
	for _, ci := range containers {
		if ci.PID <= 0 {
			continue
		}

		// Try to read iflink from the container's sysfs (via proc).
		// Path: /proc/<PID>/root/sys/class/net/eth0/iflink
		// In container mode: /host/proc/<PID>/root/sys/class/net/eth0/iflink
		procRoot := c.opts.ProcPath
		iflinks := c.findContainerIflinks(procRoot, ci.PID)

		for _, hostIfindex := range iflinks {
			if hostIface, ok := ifindexMap[hostIfindex]; ok {
				result[hostIface] = ci
			}
		}
	}

	return result
}

// findContainerIflinks reads the iflink values for all non-lo interfaces in a
// container's network namespace. Returns the host-side ifindex values.
func (c *NetworkCollector) findContainerIflinks(procPath string, pid int) []int {
	// Read from container's sysfs via /proc/<PID>/root/sys/class/net/
	containerSysNet := filepath.Join(procPath, strconv.Itoa(pid), "root", "sys", "class", "net")
	entries, err := os.ReadDir(containerSysNet)
	if err != nil {
		c.logger.Debug("cannot read container sysfs", "pid", pid, "error", err)
		return nil
	}

	var iflinks []int
	for _, entry := range entries {
		name := entry.Name()
		if name == "lo" {
			continue
		}
		iflinkStr := readFileString(filepath.Join(containerSysNet, name, "iflink"))
		if iflink, err := strconv.Atoi(iflinkStr); err == nil {
			iflinks = append(iflinks, iflink)
		}
	}
	return iflinks
}

// buildVMMapping uses virsh (if available) to map vnet/macvtap interfaces to VM names.
func (c *NetworkCollector) buildVMMapping() map[string]string {
	result := make(map[string]string)

	// Get list of running VMs.
	vmNames, err := c.runVirshListNames()
	if err != nil {
		c.logger.Debug("virsh not available, skipping VM mapping", "error", err)
		return result
	}

	// For each VM, get its network interfaces.
	for _, vmName := range vmNames {
		ifaces, err := c.runVirshDomIfList(vmName)
		if err != nil {
			c.logger.Debug("failed to get VM interfaces", "vm", vmName, "error", err)
			continue
		}
		for _, iface := range ifaces {
			result[iface] = vmName
		}
	}

	return result
}

// runVirshListNames returns the names of all running VMs.
func (c *NetworkCollector) runVirshListNames() ([]string, error) {
	cmd := c.buildCommand("virsh", "list", "--name", "--state-running")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return nil, err
	}

	var names []string
	scanner := bufio.NewScanner(&out)
	for scanner.Scan() {
		name := strings.TrimSpace(scanner.Text())
		if name != "" {
			names = append(names, name)
		}
	}
	return names, nil
}

// runVirshDomIfList returns the host-side interface names for a VM.
func (c *NetworkCollector) runVirshDomIfList(vmName string) ([]string, error) {
	cmd := c.buildCommand("virsh", "domiflist", vmName)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return nil, err
	}

	var ifaces []string
	scanner := bufio.NewScanner(&out)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		if lineNo <= 2 {
			continue // skip header + separator
		}
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) >= 1 {
			ifName := fields[0]
			// virsh domiflist shows: Interface Type Source Model MAC
			// e.g.: vnet0 bridge br0 virtio 52:54:00:...
			// or:   macvtap0 direct enp8s0f0 virtio 00:a1:98:...
			if ifName != "" && ifName != "-" {
				ifaces = append(ifaces, ifName)
			}
		}
	}
	return ifaces, nil
}

// resolveBridgeName tries to give a Docker bridge a friendlier name by
// looking up the Docker network name associated with it.
func (c *NetworkCollector) resolveBridgeName(bridgeIface string, vethMap map[string]ContainerInfo) string {
	// Docker custom networks use bridges named "br-<12char hash>".
	// Try to find a container whose network maps to this bridge.
	if !strings.HasPrefix(bridgeIface, "br-") {
		return bridgeIface
	}
	bridgeHash := strings.TrimPrefix(bridgeIface, "br-")

	for _, ci := range vethMap {
		for netName, net := range ci.Networks {
			if strings.HasPrefix(net.NetworkID, bridgeHash) {
				// Use the Docker network name as a friendlier label.
				return netName
			}
		}
	}
	return bridgeIface
}

// buildCommand creates an exec.Cmd that optionally uses chroot for container mode.
func (c *NetworkCollector) buildCommand(name string, args ...string) *exec.Cmd {
	if c.opts.IsContainer() {
		chrootArgs := append([]string{c.opts.RootfsPath, name}, args...)
		return exec.Command("chroot", chrootArgs...)
	}
	return exec.Command(name, args...)
}

// readFileString reads the entire contents of a file, returning the trimmed
// string. Returns an empty string on any error.
func readFileString(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// normalizeState converts sysfs operstate to a cleaner string.
func normalizeState(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	switch s {
	case "up", "down", "unknown":
		return s
	case "":
		return "unknown"
	default:
		return s
	}
}
