package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// DockerClient is a minimal Docker Engine API client that communicates
// over the Docker unix socket.  It only implements the subset of the API
// needed to list containers and inspect their network settings.
type DockerClient struct {
	socketPath string
	httpClient *http.Client
}

// ContainerInfo holds the subset of Docker inspect data we care about.
type ContainerInfo struct {
	ID    string
	Name  string
	PID   int
	// Networks maps Docker network name → endpoint information.
	Networks map[string]ContainerNetwork
	// Labels from the container (used for compose project detection).
	Labels map[string]string
}

// ContainerNetwork holds per-network endpoint information for a container.
type ContainerNetwork struct {
	NetworkID  string
	MacAddress string
	IPAddress  string
}

// NewDockerClient creates a client connected to the given Docker socket path.
// The socketPath should be the absolute path on the host (e.g. /var/run/docker.sock)
// or the container-mapped path (e.g. /host/var/run/docker.sock).
func NewDockerClient(socketPath string) *DockerClient {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return net.DialTimeout("unix", socketPath, 5*time.Second)
		},
	}
	return &DockerClient{
		socketPath: socketPath,
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   10 * time.Second,
		},
	}
}

// Available checks whether the Docker socket is reachable.
func (c *DockerClient) Available() bool {
	resp, err := c.httpClient.Get("http://localhost/version")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// ListContainers returns information about all running containers.
func (c *DockerClient) ListContainers() ([]ContainerInfo, error) {
	// List running containers.
	resp, err := c.httpClient.Get("http://localhost/containers/json")
	if err != nil {
		return nil, fmt.Errorf("docker list containers: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("docker read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("docker API returned %d: %s", resp.StatusCode, string(body))
	}

	var containers []dockerContainerListEntry
	if err := json.Unmarshal(body, &containers); err != nil {
		return nil, fmt.Errorf("docker unmarshal list: %w", err)
	}

	var result []ContainerInfo
	for _, c2 := range containers {
		info, err := c.inspectContainer(c2.ID)
		if err != nil {
			// Skip containers that disappear between list and inspect.
			continue
		}
		result = append(result, info)
	}
	return result, nil
}

// inspectContainer retrieves full container details via the inspect API.
func (c *DockerClient) inspectContainer(id string) (ContainerInfo, error) {
	resp, err := c.httpClient.Get(fmt.Sprintf("http://localhost/containers/%s/json", id))
	if err != nil {
		return ContainerInfo{}, fmt.Errorf("docker inspect %s: %w", id, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return ContainerInfo{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return ContainerInfo{}, fmt.Errorf("docker inspect %s returned %d", id, resp.StatusCode)
	}

	var raw dockerInspectResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return ContainerInfo{}, fmt.Errorf("docker unmarshal inspect: %w", err)
	}

	name := strings.TrimPrefix(raw.Name, "/")

	networks := make(map[string]ContainerNetwork)
	for netName, ep := range raw.NetworkSettings.Networks {
		networks[netName] = ContainerNetwork{
			NetworkID:  ep.NetworkID,
			MacAddress: ep.MacAddress,
			IPAddress:  ep.IPAddress,
		}
	}

	return ContainerInfo{
		ID:       raw.ID,
		Name:     name,
		PID:      raw.State.PID,
		Networks: networks,
		Labels:   raw.Config.Labels,
	}, nil
}

// AppName extracts a human-friendly application name from the container.
// It uses the Docker Compose project label if available, otherwise the
// container name with common prefixes stripped.
func AppName(c ContainerInfo) string {
	// Docker Compose v2 label.
	if project, ok := c.Labels["com.docker.compose.project"]; ok {
		// TrueNAS apps use "ix-<appname>" as project.
		return strings.TrimPrefix(project, "ix-")
	}
	// Fallback: strip common TrueNAS prefixes from container name.
	name := c.Name
	name = strings.TrimPrefix(name, "ix-")
	// Remove trailing instance numbers like "-1".
	if idx := strings.LastIndex(name, "-"); idx > 0 {
		suffix := name[idx+1:]
		if isNumeric(suffix) {
			name = name[:idx]
		}
	}
	return name
}

func isNumeric(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(s) > 0
}

// -- internal JSON types matching the Docker API response --

type dockerContainerListEntry struct {
	ID    string `json:"Id"`
	Names []string
}

type dockerInspectResponse struct {
	ID              string `json:"Id"`
	Name            string
	State           dockerState
	Config          dockerConfig
	NetworkSettings dockerNetworkSettings
}

type dockerState struct {
	PID int `json:"Pid"`
}

type dockerConfig struct {
	Labels map[string]string
}

type dockerNetworkSettings struct {
	Networks map[string]dockerEndpoint
}

type dockerEndpoint struct {
	NetworkID  string
	MacAddress string
	IPAddress  string
}
