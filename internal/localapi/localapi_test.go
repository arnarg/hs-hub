package localapi

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"path/filepath"
	"slices"
	"testing"

	"github.com/arnarg/hs-hub/internal/hub"
)

// Trimmed from a real `tailscale status --json`.
const statusJSON = `{
  "Version": "1.90.0",
  "BackendState": "Running",
  "Self": {
    "ID": "nSelf",
    "HostName": "sky",
    "DNSName": "sky.ts.example.com.",
    "TailscaleIPs": ["100.64.0.1", "fd7a:115c:a1e0::1"],
    "Tags": ["tag:hub", "tag:server"],
    "Online": true
  },
  "Peer": {
    "nodekey:aaa": {
      "ID": "nLab",
      "HostName": "lab",
      "TailscaleIPs": ["100.64.0.2", "fd7a:115c:a1e0::2"],
      "Tags": ["tag:server"],
      "Online": true
    },
    "nodekey:bbb": {
      "ID": "nPhone",
      "HostName": "phone",
      "TailscaleIPs": ["100.64.0.3"],
      "Online": false
    }
  }
}`

func serve(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "tailscaled.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return sock
}

func TestPeers(t *testing.T) {
	sock := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "local-tailscaled.sock" {
			http.Error(w, "bad host", http.StatusForbidden)
			return
		}
		if r.URL.Path != "/localapi/v0/status" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(statusJSON))
	})

	peers, err := New(sock).Peers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(peers, func(a, b hub.Peer) int {
		if a.ID < b.ID {
			return -1
		}
		return 1
	})

	if len(peers) != 3 {
		t.Fatalf("got %d peers: %+v", len(peers), peers)
	}
	lab := peers[0]
	if lab.ID != "nLab" || lab.HostName != "lab" || !slices.Equal(lab.Tags, []string{"tag:server"}) {
		t.Fatalf("lab = %+v", lab)
	}
	if !slices.Equal(lab.IPs, []netip.Addr{netip.MustParseAddr("100.64.0.2"), netip.MustParseAddr("fd7a:115c:a1e0::2")}) {
		t.Fatalf("lab IPs = %v", lab.IPs)
	}
	if peers[2].ID != "nSelf" || !slices.Contains(peers[2].Tags, "tag:server") {
		t.Fatalf("self = %+v", peers[2])
	}
}

func TestPeersNotRunning(t *testing.T) {
	sock := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"BackendState": "NeedsLogin"}`))
	})
	if _, err := New(sock).Peers(context.Background()); err == nil {
		t.Fatal("expected error")
	}
}

func TestPeersHTTPError(t *testing.T) {
	sock := serve(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	})
	if _, err := New(sock).Peers(context.Background()); err == nil {
		t.Fatal("expected error")
	}
}
