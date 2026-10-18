package collector

import (
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"
)

// shortIDLength is the length of the container ID prefix that Docker prints and that TrueNAS
// reporting (netdata) uses in cgroup chart names such as "cgroup_795d18598fc4".
const shortIDLength = 12

// ContainerCollector exports one info series per running Docker container, mapping its short ID
// to its name and application. Dashboards join on it to show app names for data keyed by ID.
type ContainerCollector struct {
	dockerSocket string
	logger       *slog.Logger
	info         *prometheus.Desc
}

// NewContainerCollector creates a ContainerCollector that reads containers from the Docker socket.
func NewContainerCollector(logger *slog.Logger, dockerSocket string) *ContainerCollector {
	return &ContainerCollector{
		dockerSocket: dockerSocket,
		logger:       logger,
		info: prometheus.NewDesc(
			"truenas_docker_container_info",
			"Running Docker container, always 1. id is the 12-character short ID used in cgroup chart names.",
			[]string{"id", "name", "app"}, nil,
		),
	}
}

// Describe implements prometheus.Collector.
func (c *ContainerCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.info
}

// Collect implements prometheus.Collector.
func (c *ContainerCollector) Collect(ch chan<- prometheus.Metric) {
	containers, err := NewDockerClient(c.dockerSocket).ListContainerSummaries()
	if err != nil {
		c.logger.Debug("docker unavailable, skipping container info", "error", err)
		return
	}
	for _, container := range containers {
		id := container.ID
		if len(id) > shortIDLength {
			id = id[:shortIDLength]
		}
		ch <- prometheus.MustNewConstMetric(c.info, prometheus.GaugeValue, 1, id, container.Name, AppName(container))
	}
}
