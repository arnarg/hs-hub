package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"time"
)

// maxManifestSize caps how much of a manifest response is read.
const maxManifestSize = 1 << 20

// Manifest is the document each node serves, e.g.
//
//	{"services": ["reader.example.com", "db.example.com"]}
type Manifest struct {
	Services []string `json:"services"`
}

// HTTPFetcher fetches manifests over plain HTTP. Transport security comes
// from the tailnet itself.
type HTTPFetcher struct {
	Port    uint16
	Path    string
	Timeout time.Duration
	Client  *http.Client
}

func (f *HTTPFetcher) Fetch(ctx context.Context, addr netip.Addr) ([]string, error) {
	if f.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, f.Timeout)
		defer cancel()
	}

	url := fmt.Sprintf("http://%s%s", netip.AddrPortFrom(addr, f.Port), f.Path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	client := f.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %s", resp.Status)
	}

	var m Manifest
	dec := json.NewDecoder(io.LimitReader(resp.Body, maxManifestSize))
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("decoding manifest: %w", err)
	}
	if m.Services == nil {
		m.Services = []string{}
	}
	return m.Services, nil
}
