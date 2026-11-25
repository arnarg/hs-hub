// Package localapi lists tailnet peers through tailscaled's LocalAPI.
//
// It talks to the unix socket directly instead of importing tailscale.com,
// relying only on the long-stable shape of /localapi/v0/status (the same
// document `tailscale status --json` prints).
package localapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"

	"github.com/arnarg/hs-hub/internal/hub"
)

// DefaultSocket is tailscaled's LocalAPI socket on Linux.
const DefaultSocket = "/run/tailscale/tailscaled.sock"

// tailscaled rejects LocalAPI requests with any other Host header.
const localAPIHost = "local-tailscaled.sock"

// Client implements hub.PeerSource.
type Client struct {
	http *http.Client
}

// New returns a client for the LocalAPI listening on socketPath.
func New(socketPath string) *Client {
	var d net.Dialer
	return &Client{
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return d.DialContext(ctx, "unix", socketPath)
				},
			},
		},
	}
}

type peerStatus struct {
	ID           string
	HostName     string
	TailscaleIPs []netip.Addr
	Tags         []string
}

type status struct {
	BackendState string
	Self         *peerStatus
	Peer         map[string]*peerStatus
}

// Peers returns the local node followed by all peers in the netmap.
func (c *Client) Peers(ctx context.Context) ([]hub.Peer, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+localAPIHost+"/localapi/v0/status", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("localapi status: %s: %s", resp.Status, body)
	}

	var st status
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return nil, fmt.Errorf("decoding localapi status: %w", err)
	}
	if st.BackendState != "Running" {
		return nil, fmt.Errorf("tailscaled is not running (state %q)", st.BackendState)
	}

	peers := make([]hub.Peer, 0, len(st.Peer)+1)
	if st.Self != nil {
		peers = append(peers, toPeer(st.Self))
	}
	for _, p := range st.Peer {
		if p != nil {
			peers = append(peers, toPeer(p))
		}
	}
	return peers, nil
}

func toPeer(p *peerStatus) hub.Peer {
	return hub.Peer{
		ID:       p.ID,
		HostName: p.HostName,
		IPs:      p.TailscaleIPs,
		Tags:     p.Tags,
	}
}
