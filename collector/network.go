package collector

import (
	"bufio"
	"bytes"
	"encoding/json"
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
	VLAN         string // 802.1Q VLAN ID (inherited from bridge uplink if applicable)
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
	labels := []string{"interface", "instance", "instance_type", "app", "bridge", "vlan", "state"}

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
	// 1. Read interface stats from /proc/1/net/dev (host network namespace).
	stats, err := c.readProcNetDev()
	if err != nil {
		c.logger.Error("failed to read /proc/1/net/dev", "error", err)
		return
	}

	c.logger.Debug("collected interface stats", "count", len(stats))

	// 2. Build interface → metadata mapping.
	infoMap := c.buildInterfaceInfo(stats)

	// 3. Emit metrics.
	for iface, s := range stats {
		info, ok := infoMap[iface]
		if !ok {
			continue
		}

		labels := []string{info.Name, info.Instance, info.InstanceType, info.App, info.Bridge, info.VLAN, info.State}

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
// Note: /proc/net is a symlink to /proc/self/net which resolves to the
// current process's network namespace. In a container, this would show
// only the container's interfaces. We use /proc/1/net/dev instead, as
// PID 1 (host init) is always in the host's network namespace.
func (c *NetworkCollector) readProcNetDev() (map[string]interfaceStats, error) {
	path := filepath.Join(c.opts.ProcPath, "1", "net", "dev")
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

	// Query Docker for container → veth mapping and network → bridge mapping.
	vethToContainer, bridgeToNetwork := c.fetchDockerData(ifindexMap)

	// Query Incus/LXC for container → veth mapping.
	vethToIncus := c.buildIncusMapping(ifindexMap)

	// Query midclt/virsh for VM → vnet mapping.
	vnetToVM := c.buildVMMapping()

	// Parse VLAN sub-interfaces from /proc/net/vlan/config.
	vlanMap := c.buildVLANMap()

	// Build bridge → VLAN mapping: for each bridge, find the VLAN ID of any
	// VLAN sub-interface that is a member of that bridge.
	bridgeVLAN := make(map[string]string)
	for iface, vi := range vlanMap {
		if br, ok := bridgeMap[iface]; ok {
			bridgeVLAN[br] = vi.ID
		}
	}

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
			info.App = "system"

		case strings.HasPrefix(iface, "veth"):
			// Container veth — check Docker first, then Incus/LXC.
			if ci, ok := vethToContainer[iface]; ok {
				info.InstanceType = "docker"
				info.Instance = ci.Name
				info.App = AppName(ci)
			} else if incusName, ok := vethToIncus[iface]; ok {
				info.InstanceType = "incus"
				info.Instance = incusName
				info.App = incusName
			} else {
				info.InstanceType = "docker"
				info.Instance = iface
				// Derive app from the parent bridge's Docker network.
				if br, ok := bridgeMap[iface]; ok {
					if netInfo, ok := bridgeToNetwork[br]; ok {
						info.App = appNameFromDockerNetwork(netInfo.Name)
					}
				}
			}
			// Inherit VLAN from parent bridge.
			if br := bridgeMap[iface]; br != "" {
				info.VLAN = bridgeVLAN[br]
			}

		case strings.HasPrefix(iface, "vnet"):
			// VM network interface (libvirt tap/tun).
			info.InstanceType = "vm"
			if vmName, ok := vnetToVM[iface]; ok {
				info.Instance = vmName
				info.App = vmName
			} else {
				info.Instance = iface
			}
			// Inherit VLAN from parent bridge.
			if br := bridgeMap[iface]; br != "" {
				info.VLAN = bridgeVLAN[br]
			}

		case strings.HasPrefix(iface, "macvtap") || strings.HasPrefix(iface, "macvlan"):
			info.InstanceType = "macvtap"
			if vmName, ok := vnetToVM[iface]; ok {
				info.Instance = vmName
				info.App = vmName
			} else {
				info.Instance = iface
			}

		case strings.HasPrefix(iface, "vlan"):
			info.InstanceType = "vlan"
			info.Instance = iface
			info.App = "system"
			if vi, ok := vlanMap[iface]; ok {
				info.VLAN = vi.ID
			}

		case strings.HasPrefix(iface, "br-") || strings.HasPrefix(iface, "br") ||
			strings.HasPrefix(iface, "docker") || strings.HasPrefix(iface, "incus"):
			info.InstanceType = "bridge"
			info.VLAN = bridgeVLAN[iface]
			// Only use Docker network name for hash-named bridges (br-<hash>).
			// Well-known bridges (br0, docker0, incusbr0) keep their own name.
			if strings.HasPrefix(iface, "br-") {
				if netInfo, ok := bridgeToNetwork[iface]; ok {
					info.Instance = netInfo.Name
					info.App = appNameFromDockerNetwork(netInfo.Name)
				} else {
					info.Instance = iface
				}
			} else {
				info.Instance = iface
				info.App = "system"
			}

		default:
			// Check if it's a physical device (has a device/driver symlink in sysfs).
			driverPath := filepath.Join(sysNetPath, iface, "device", "driver")
			if _, err := os.Readlink(driverPath); err == nil {
				info.InstanceType = "physical"
			} else {
				info.InstanceType = "unknown"
			}
			info.Instance = iface
			info.App = "system"
			// Check if this is a VLAN sub-interface (e.g. eno1.100).
			if vi, ok := vlanMap[iface]; ok {
				info.InstanceType = "vlan"
				info.VLAN = vi.ID
			}
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

// vlanInfo describes one 802.1Q VLAN sub-interface.
type vlanInfo struct {
	ID     string // VLAN ID (e.g., "100")
	Parent string // Parent device (e.g., "eno1")
}

// buildVLANMap parses /proc/net/vlan/config to discover 802.1Q VLAN
// sub-interfaces. Returns a map from interface name to VLAN info.
//
// Format of /proc/net/vlan/config:
//
//	VLAN Dev name       | VLAN ID
//	Name-Type: VLAN_NAME_TYPE_RAW_PLUS_VID_NO_PAD
//	eno1.100           | 100  | eno1
func (c *NetworkCollector) buildVLANMap() map[string]vlanInfo {
	result := make(map[string]vlanInfo)

	path := filepath.Join(c.opts.ProcPath, "1", "net", "vlan", "config")
	f, err := os.Open(path)
	if err != nil {
		c.logger.Debug("VLAN config not available", "path", path, "error", err)
		return result
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		// Skip headers and blank lines.
		if strings.HasPrefix(line, "VLAN") || strings.HasPrefix(line, "Name-Type:") || strings.TrimSpace(line) == "" {
			continue
		}
		// Parse: "<dev_name> | <vlan_id> | <parent_dev>"
		parts := strings.Split(line, "|")
		if len(parts) < 3 {
			continue
		}
		devName := strings.TrimSpace(parts[0])
		vlanID := strings.TrimSpace(parts[1])
		parent := strings.TrimSpace(parts[2])
		if devName != "" && vlanID != "" {
			result[devName] = vlanInfo{ID: vlanID, Parent: parent}
		}
	}

	if len(result) > 0 {
		c.logger.Debug("discovered VLAN interfaces", "count", len(result))
	}

	return result
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

// fetchDockerData queries the Docker API and returns:
// 1. A mapping from host-side veth interfaces to their owning containers.
// 2. A mapping from bridge interface names to their Docker network info.
func (c *NetworkCollector) fetchDockerData(ifindexMap map[int]string) (map[string]ContainerInfo, map[string]DockerNetworkInfo) {
	vethMap := make(map[string]ContainerInfo)
	netMap := make(map[string]DockerNetworkInfo)

	client := NewDockerClient(c.dockerSocket)
	if !client.Available() {
		c.logger.Debug("docker socket not available, skipping container/network mapping")
		return vethMap, netMap
	}

	// Map containers to their host-side veth interfaces.
	containers, err := client.ListContainers()
	if err != nil {
		c.logger.Warn("failed to list docker containers", "error", err)
	} else {
		for _, ci := range containers {
			if ci.PID <= 0 {
				continue
			}
			iflinks := c.findContainerIflinks(c.opts.ProcPath, ci.PID)
			for _, hostIfindex := range iflinks {
				if hostIface, ok := ifindexMap[hostIfindex]; ok {
					vethMap[hostIface] = ci
				}
			}
		}
	}

	// Map Docker bridge interfaces to their network names.
	networks, err := client.ListNetworks()
	if err != nil {
		c.logger.Warn("failed to list docker networks", "error", err)
	} else {
		for _, n := range networks {
			if n.BridgeName != "" {
				netMap[n.BridgeName] = n
			}
		}
	}

	return vethMap, netMap
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

// buildVMMapping maps vnet/macvtap interfaces to VM names.
// It first tries the TrueNAS midclt API, then falls back to virsh.
func (c *NetworkCollector) buildVMMapping() map[string]string {
	result := make(map[string]string)

	// Try TrueNAS midclt API first (works on TrueNAS SCALE where virsh is unavailable).
	if vms, err := c.queryMidcltVMs(); err == nil && len(vms) > 0 {
		for _, vm := range vms {
			if vm.pid <= 0 {
				continue
			}
			ifaces := c.findQEMUInterfaces(vm.pid)
			for _, iface := range ifaces {
				result[iface] = vm.name
			}
		}
		if len(result) > 0 {
			c.logger.Debug("mapped VMs via midclt", "count", len(result))
			return result
		}
	}

	// Fall back to virsh.
	vmNames, err := c.runVirshListNames()
	if err != nil {
		c.logger.Debug("vm mapping not available (neither midclt nor virsh)", "error", err)
		return result
	}

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

// vmEntry holds a running VM's name and QEMU PID.
type vmEntry struct {
	name string
	pid  int
}

// queryMidcltVMs queries the TrueNAS middleware for running VMs.
func (c *NetworkCollector) queryMidcltVMs() ([]vmEntry, error) {
	cmd := c.buildCommand("midclt", "call", "vm.query")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return nil, err
	}

	var raw []struct {
		Name   string `json:"name"`
		Status struct {
			State string `json:"state"`
			PID   int    `json:"pid"`
		} `json:"status"`
	}
	if err := json.Unmarshal(out.Bytes(), &raw); err != nil {
		return nil, fmt.Errorf("midclt unmarshal: %w", err)
	}

	var vms []vmEntry
	for _, r := range raw {
		if r.Status.State == "RUNNING" && r.Status.PID > 0 {
			vms = append(vms, vmEntry{name: r.Name, pid: r.Status.PID})
		}
	}
	return vms, nil
}

// findQEMUInterfaces scans /proc/<PID>/fd and fdinfo to discover
// the tap/macvtap interfaces owned by a QEMU process.
// - tap devices: /dev/net/tun FDs with "iff: vnetX" in fdinfo
// - macvtap devices: /dev/tapN FDs where N is the ifindex
func (c *NetworkCollector) findQEMUInterfaces(pid int) []string {
	fdDir := filepath.Join(c.opts.ProcPath, strconv.Itoa(pid), "fd")
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		c.logger.Debug("cannot read QEMU fd dir", "pid", pid, "error", err)
		return nil
	}

	var ifaces []string
	for _, entry := range entries {
		fdPath := filepath.Join(fdDir, entry.Name())
		target, err := os.Readlink(fdPath)
		if err != nil {
			continue
		}

		switch {
		case target == "/dev/net/tun":
			// Read fdinfo for the interface name ("iff:\tvnetX").
			fdinfoPath := filepath.Join(c.opts.ProcPath, strconv.Itoa(pid), "fdinfo", entry.Name())
			if ifName := readFdinfoIff(fdinfoPath); ifName != "" {
				ifaces = append(ifaces, ifName)
			}

		case strings.HasPrefix(target, "/dev/tap"):
			// macvtap: /dev/tapN where N = ifindex of the macvtap interface.
			idxStr := strings.TrimPrefix(target, "/dev/tap")
			if idx, err := strconv.Atoi(idxStr); err == nil {
				if ifName := c.resolveIfindex(idx); ifName != "" {
					ifaces = append(ifaces, ifName)
				}
			}
		}
	}
	return ifaces
}

// readFdinfoIff reads the "iff:" line from a /proc/<PID>/fdinfo/<FD> file.
// Returns the interface name (e.g. "vnet0") or empty string.
func readFdinfoIff(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "iff:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "iff:"))
		}
	}
	return ""
}

// resolveIfindex finds the interface name for a given ifindex by scanning sysfs.
func (c *NetworkCollector) resolveIfindex(idx int) string {
	sysNetPath := c.sysClassNetPath()
	entries, err := os.ReadDir(sysNetPath)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		idxStr := readFileString(filepath.Join(sysNetPath, entry.Name(), "ifindex"))
		if ifidx, err := strconv.Atoi(idxStr); err == nil && ifidx == idx {
			return entry.Name()
		}
	}
	return ""
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
			if ifName != "" && ifName != "-" {
				ifaces = append(ifaces, ifName)
			}
		}
	}
	return ifaces, nil
}

// appNameFromDockerNetwork extracts a TrueNAS app name from a Docker
// network name. TrueNAS apps create networks named "ix-<appname>_<suffix>".
func appNameFromDockerNetwork(networkName string) string {
	if !strings.HasPrefix(networkName, "ix-") {
		return ""
	}
	name := strings.TrimPrefix(networkName, "ix-")
	if idx := strings.Index(name, "_"); idx > 0 {
		return name[:idx]
	}
	return name
}

// buildIncusMapping discovers Incus/LXC containers by scanning /proc for
// processes in LXC cgroups and maps their host-side veth interfaces.
//
// LXC containers have a cgroup path like:
//
//	0::/lxc.payload.<containername>/init.scope
//
// We look for init processes (the ones with /init.scope) and use the same
// iflink technique as Docker to find their host-side veth interfaces.
func (c *NetworkCollector) buildIncusMapping(ifindexMap map[int]string) map[string]string {
	result := make(map[string]string)

	procDir := c.opts.ProcPath
	entries, err := os.ReadDir(procDir)
	if err != nil {
		return result
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 1 {
			continue
		}

		// Read the cgroup file to check for LXC container init processes.
		cgroupData := readFileString(filepath.Join(procDir, entry.Name(), "cgroup"))
		if cgroupData == "" {
			continue
		}

		// Look for the pattern "lxc.payload.<name>/init.scope".
		// Only match init.scope to avoid scanning all container processes.
		containerName := parseLXCCgroup(cgroupData)
		if containerName == "" {
			continue
		}

		// Skip if we already mapped this container (multiple init.scope PIDs).
		alreadyMapped := false
		for _, name := range result {
			if name == containerName {
				alreadyMapped = true
				break
			}
		}
		if alreadyMapped {
			continue
		}

		// Find host-side veth interfaces via iflink.
		iflinks := c.findContainerIflinks(procDir, pid)
		for _, hostIfindex := range iflinks {
			if hostIface, ok := ifindexMap[hostIfindex]; ok {
				result[hostIface] = containerName
			}
		}
	}

	if len(result) > 0 {
		c.logger.Debug("mapped Incus/LXC containers", "count", len(result))
	}

	return result
}

// parseLXCCgroup extracts the LXC container name from a cgroup file content.
// Returns empty string if not an LXC container init process.
// Expected format: "0::/lxc.payload.backupserver/init.scope"
func parseLXCCgroup(data string) string {
	for _, line := range strings.Split(data, "\n") {
		idx := strings.Index(line, "lxc.payload.")
		if idx < 0 {
			continue
		}
		rest := line[idx+len("lxc.payload."):]
		slashIdx := strings.Index(rest, "/")
		if slashIdx <= 0 {
			continue
		}
		// Only match init processes to avoid duplicates.
		suffix := rest[slashIdx:]
		if suffix != "/init.scope" {
			continue
		}
		return rest[:slashIdx]
	}
	return ""
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
