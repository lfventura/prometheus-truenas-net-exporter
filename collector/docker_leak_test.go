package collector

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// countingDocker serves the Docker endpoints the collectors call on a unix socket and counts the
// connections that are open at the end.
func countingDocker(t *testing.T) (string, *atomic.Int64) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "docker.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var open atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			io.WriteString(w, `{}`)
			return
		}
		io.WriteString(w, `[]`)
	})
	srv := &http.Server{Handler: mux, ConnState: func(_ net.Conn, s http.ConnState) {
		switch s {
		case http.StateNew:
			open.Add(1)
		case http.StateClosed, http.StateHijacked:
			open.Add(-1)
		}
	}}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return sock, &open
}

func drain(c prometheus.Collector) {
	ch := make(chan prometheus.Metric, 1024)
	go func() { c.Collect(ch); close(ch) }()
	for range ch {
	}
}

// Repeated scrapes must reuse the collector's Docker connection instead of leaving one open per scrape.
func TestScrapesDoNotLeakDockerConnections(t *testing.T) {
	sock, open := countingDocker(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cc := NewContainerCollector(logger, sock)
	for i := 0; i < 200; i++ {
		drain(cc)
		if _, err := cc.docker.ListNetworks(); err != nil { // the network collector's calls on the same kind of client
			t.Fatal(err)
		}
	}
	nc := NewNetworkCollector(logger, Options{}, sock)
	for i := 0; i < 200; i++ {
		nc.fetchDockerData(map[int]string{})
	}
	if n := open.Load(); n > 4 {
		t.Fatalf("%d docker connections open after 400 scrapes, want at most 4", n)
	}
}
