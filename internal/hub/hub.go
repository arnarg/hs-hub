// Package hub discovers services published by tailnet nodes and renders them
// into a Headscale extra records file.
//
// Every node carrying the configured tag serves a manifest over HTTP on the
// tailnet listing the DNS names it serves. Each round the hub lists the
// tailnet peers, fetches the manifests of the tagged ones, and writes A/AAAA
// records pointing each name at the tailnet IPs of the node that claimed it.
package hub

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

// Peer is a node on the tailnet as seen by the local tailscaled.
type Peer struct {
	ID       string
	HostName string
	IPs      []netip.Addr
	Tags     []string
}

// PeerSource lists the nodes on the tailnet, including the local node.
type PeerSource interface {
	Peers(ctx context.Context) ([]Peer, error)
}

// ManifestFetcher fetches the list of names a node publishes.
type ManifestFetcher interface {
	Fetch(ctx context.Context, addr netip.Addr) ([]string, error)
}

// Record is a single entry in Headscale's extra records file. It mirrors
// tailcfg.DNSRecord.
type Record struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Value string `json:"value"`
}

// Config controls which nodes are queried and which names are accepted.
type Config struct {
	// Tag that nodes must carry for their manifest to be fetched.
	Tag string
	// Domains that published names must be strictly below.
	Domains []string
	// ExcludeDomains are rejected even when below an allowed domain, e.g. the
	// MagicDNS base domain so nodes cannot shadow MagicDNS names.
	ExcludeDomains []string
	// Reserved names are never published, e.g. names with public DNS records
	// such as the Headscale server itself.
	Reserved []string
	// Grace is how long a node's last good manifest is kept after it stops
	// answering or disappears from the tailnet.
	Grace time.Duration
	// Concurrency limits parallel manifest fetches.
	Concurrency int
	// OutputPath is the records file Headscale reads (dns.extra_records_path).
	OutputPath string
	// StatePath optionally persists known nodes across restarts.
	StatePath string

	reserved map[string]struct{}
}

type nodeState struct {
	HostName string       `json:"hostName"`
	IPs      []netip.Addr `json:"ips"`
	Names    []string     `json:"names"`
	LastOK   time.Time    `json:"lastOK"`
}

// Hub holds the known nodes and writes the records file.
type Hub struct {
	cfg     Config
	peers   PeerSource
	fetcher ManifestFetcher
	log     *slog.Logger
	now     func() time.Time

	mu        sync.Mutex
	nodes     map[string]*nodeState // keyed by stable node ID
	lastState []byte
	// failing holds the IDs of tagged nodes whose last fetch failed, so a
	// failure is only logged when a node goes from working to failing.
	failing map[string]struct{}
}

// New creates a hub, loading persisted state and the current records file if
// they exist.
func New(cfg Config, peers PeerSource, fetcher ManifestFetcher, log *slog.Logger) (*Hub, error) {
	if cfg.Tag == "" {
		return nil, errors.New("tag must be set")
	}
	if len(cfg.Domains) == 0 {
		return nil, errors.New("at least one domain must be set")
	}
	if cfg.OutputPath == "" {
		return nil, errors.New("output path must be set")
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 16
	}
	for i, d := range cfg.Domains {
		cfg.Domains[i] = normalizeName(d)
	}
	for i, d := range cfg.ExcludeDomains {
		cfg.ExcludeDomains[i] = normalizeName(d)
	}
	cfg.reserved = make(map[string]struct{}, len(cfg.Reserved))
	for _, r := range cfg.Reserved {
		cfg.reserved[normalizeName(r)] = struct{}{}
	}

	h := &Hub{
		cfg:     cfg,
		peers:   peers,
		fetcher: fetcher,
		log:     log,
		now:     time.Now,
		nodes:   map[string]*nodeState{},
		failing: map[string]struct{}{},
	}

	if cfg.StatePath != "" {
		b, err := os.ReadFile(cfg.StatePath)
		switch {
		case err == nil:
			if err := json.Unmarshal(b, &h.nodes); err != nil {
				log.Warn("ignoring unreadable state file", "path", cfg.StatePath, "err", err)
				h.nodes = map[string]*nodeState{}
			} else {
				h.lastState = b
				log.Info("loaded state", "nodes", len(h.nodes))
			}
		case !errors.Is(err, fs.ErrNotExist):
			return nil, fmt.Errorf("reading state file: %w", err)
		}
	}

	return h, nil
}

// Round lists the tailnet, fetches manifests from tagged nodes and rewrites
// the records file if the result changed.
func (h *Hub) Round(ctx context.Context) error {
	peers, err := h.peers.Peers(ctx)
	if err != nil {
		// Without a peer list we know nothing new; keep everything as is
		// rather than letting nodes expire.
		return fmt.Errorf("listing peers: %w", err)
	}

	var targets []Peer
	for _, p := range peers {
		if !slices.Contains(p.Tags, h.cfg.Tag) {
			h.mu.Lock()
			if _, known := h.nodes[p.ID]; known {
				// Still on the tailnet but no longer tagged: drop at once.
				h.log.Info("node lost tag, dropping its names", "host", p.HostName)
				delete(h.nodes, p.ID)
			}
			h.mu.Unlock()
			continue
		}
		if _, ok := fetchAddr(p.IPs); !ok {
			h.log.Warn("node has no tailnet address", "host", p.HostName)
			continue
		}
		targets = append(targets, p)
	}

	var wg sync.WaitGroup
	sem := make(chan struct{}, h.cfg.Concurrency)
	for _, p := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			addr, _ := fetchAddr(p.IPs)
			names, err := h.fetcher.Fetch(ctx, addr)
			h.mu.Lock()
			defer h.mu.Unlock()
			if err != nil {
				if _, already := h.failing[p.ID]; already {
					h.log.Debug("fetching manifest still failing", "host", p.HostName, "addr", addr, "err", err)
				} else {
					h.failing[p.ID] = struct{}{}
					h.log.Warn("fetching manifest failed", "host", p.HostName, "addr", addr, "err", err)
				}
				return
			}
			if _, was := h.failing[p.ID]; was {
				delete(h.failing, p.ID)
				h.log.Info("fetching manifest recovered", "host", p.HostName, "addr", addr)
			}
			h.nodes[p.ID] = &nodeState{
				HostName: p.HostName,
				IPs:      slices.Clone(p.IPs),
				Names:    names,
				LastOK:   h.now(),
			}
		}()
	}
	wg.Wait()

	h.mu.Lock()
	defer h.mu.Unlock()

	// Forget failures of nodes that are no longer queried, so a node that
	// comes back later is logged afresh.
	for id := range h.failing {
		if !slices.ContainsFunc(targets, func(p Peer) bool { return p.ID == id }) {
			delete(h.failing, id)
		}
	}

	h.expire()
	if err := h.writeRecords(h.build()); err != nil {
		return err
	}
	return h.saveState()
}

// fetchAddr picks the address used to reach a node, preferring IPv4.
func fetchAddr(ips []netip.Addr) (netip.Addr, bool) {
	for _, ip := range ips {
		if ip.Is4() {
			return ip, true
		}
	}
	if len(ips) > 0 {
		return ips[0], true
	}
	return netip.Addr{}, false
}

// expire drops nodes whose last good manifest is older than the grace
// period. Must be called with h.mu held.
func (h *Hub) expire() {
	now := h.now()
	for id, n := range h.nodes {
		if now.Sub(n.LastOK) > h.cfg.Grace {
			h.log.Info("node expired, dropping its names", "host", n.HostName, "lastOK", n.LastOK)
			delete(h.nodes, id)
		}
	}
}

// build renders the records for all known nodes. Conflicting claims are
// resolved by host name (then node ID) so the result is deterministic. Must
// be called with h.mu held.
func (h *Hub) build() []Record {
	ids := slices.SortedFunc(maps.Keys(h.nodes), func(a, b string) int {
		return cmp.Or(cmp.Compare(h.nodes[a].HostName, h.nodes[b].HostName), cmp.Compare(a, b))
	})

	owner := map[string]string{}
	recs := []Record{}
	for _, id := range ids {
		n := h.nodes[id]
		for _, raw := range n.Names {
			name := normalizeName(raw)
			if reason := h.cfg.checkName(name); reason != "" {
				h.log.Warn("rejecting name", "host", n.HostName, "name", raw, "reason", reason)
				continue
			}
			if o, taken := owner[name]; taken {
				if o != n.HostName {
					h.log.Warn("name conflict", "name", name, "owner", o, "rejected", n.HostName)
				}
				continue
			}
			owner[name] = n.HostName
			for _, ip := range n.IPs {
				typ := "A"
				if ip.Is6() {
					typ = "AAAA"
				}
				recs = append(recs, Record{Name: name, Type: typ, Value: ip.String()})
			}
		}
	}

	slices.SortFunc(recs, func(a, b Record) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.Type, b.Type), cmp.Compare(a.Value, b.Value))
	})
	return recs
}

func (h *Hub) writeRecords(recs []Record) error {
	b, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	// Compare with what is on disk rather than what was last written, so a
	// deleted or hand-edited file is repaired on the next round.
	if cur, err := os.ReadFile(h.cfg.OutputPath); err == nil && bytes.Equal(cur, b) {
		return nil
	}
	if err := writeAtomic(h.cfg.OutputPath, b, 0o644); err != nil {
		return fmt.Errorf("writing records file: %w", err)
	}
	h.log.Info("wrote records file", "path", h.cfg.OutputPath, "records", len(recs))
	return nil
}

func (h *Hub) saveState() error {
	if h.cfg.StatePath == "" {
		return nil
	}
	b, err := json.MarshalIndent(h.nodes, "", "  ")
	if err != nil {
		return err
	}
	if bytes.Equal(b, h.lastState) {
		return nil
	}
	if err := writeAtomic(h.cfg.StatePath, b, 0o644); err != nil {
		return fmt.Errorf("writing state file: %w", err)
	}
	h.lastState = b
	return nil
}

// writeAtomic writes data to a temporary file next to path and renames it
// into place, so readers never observe a partial file. Headscale re-adds its
// file watch after the rename.
func writeAtomic(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after a successful rename

	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
