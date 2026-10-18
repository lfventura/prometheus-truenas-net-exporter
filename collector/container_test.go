package collector

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// fakeDocker serves a Docker Engine API container list on a unix socket.
func fakeDocker(t *testing.T, body string) string {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "docker.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/containers/json" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, body)
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return socket
}

func TestContainerCollector(t *testing.T) {
	socket := fakeDocker(t, `[
		{"Id": "795d18598fc4187fb44a01e2d3fa35bc041f79977f3e12b5d2cf4c9eb395753c", "Names": ["/ix-jellyfin-jellyfin-1"],
		 "Labels": {"com.docker.compose.project": "ix-jellyfin"}},
		{"Id": "3a1f0c2b9d8e7f6a5b4c3d2e1f0a9b8c7d6e5f4a3b2c1d0e9f8a7b6c5d4e3f2a", "Names": ["/wyoming-whisper"], "Labels": {}}
	]`)
	collector := NewContainerCollector(slog.New(slog.NewTextHandler(io.Discard, nil)), socket)

	want := `
# HELP truenas_docker_container_info Running Docker container, always 1. id is the 12-character short ID used in cgroup chart names.
# TYPE truenas_docker_container_info gauge
truenas_docker_container_info{app="jellyfin",id="795d18598fc4",name="ix-jellyfin-jellyfin-1"} 1
truenas_docker_container_info{app="wyoming-whisper",id="3a1f0c2b9d8e",name="wyoming-whisper"} 1
`
	if err := testutil.CollectAndCompare(collector, strings.NewReader(want)); err != nil {
		t.Fatal(err)
	}
}

func TestContainerCollectorWithoutDocker(t *testing.T) {
	collector := NewContainerCollector(slog.New(slog.NewTextHandler(io.Discard, nil)), filepath.Join(t.TempDir(), "missing.sock"))
	if n := testutil.CollectAndCount(collector); n != 0 {
		t.Fatalf("got %d series without Docker, want 0", n)
	}
}
