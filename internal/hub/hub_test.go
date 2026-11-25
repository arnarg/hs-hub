package hub

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

type fakePeers struct {
	peers []Peer
	err   error
}

func (f *fakePeers) Peers(context.Context) ([]Peer, error) { return f.peers, f.err }

type fakeFetcher struct {
	mu      sync.Mutex
	answers map[netip.Addr][]string // missing address means fetch fails
}

func (f *fakeFetcher) Fetch(_ context.Context, addr netip.Addr) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	names, ok := f.answers[addr]
	if !ok {
		return nil, errors.New("connection refused")
	}
	return names, nil
}

func peer(id, host, v4, v6 string, tags ...string) Peer {
	p := Peer{ID: id, HostName: host, Tags: tags}
	if v4 != "" {
		p.IPs = append(p.IPs, netip.MustParseAddr(v4))
	}
	if v6 != "" {
		p.IPs = append(p.IPs, netip.MustParseAddr(v6))
	}
	return p
}

type env struct {
	t       *testing.T
	dir     string
	cfg     Config
	peers   *fakePeers
	fetcher *fakeFetcher
	clock   time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	return &env{
		t:   t,
		dir: dir,
		cfg: Config{
			Tag:            "tag:server",
			Domains:        []string{"example.com"},
			ExcludeDomains: []string{"ts.example.com"},
			Reserved:       []string{"hs.example.com"},
			Grace:          10 * time.Minute,
			OutputPath:     filepath.Join(dir, "records.json"),
			StatePath:      filepath.Join(dir, "state.json"),
		},
		peers:   &fakePeers{},
		fetcher: &fakeFetcher{answers: map[netip.Addr][]string{}},
		clock:   time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

func (e *env) hub() *Hub {
	e.t.Helper()
	h, err := New(e.cfg, e.peers, e.fetcher, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		e.t.Fatal(err)
	}
	h.now = func() time.Time { return e.clock }
	return h
}

func (e *env) serve(ip string, names ...string) {
	e.fetcher.mu.Lock()
	defer e.fetcher.mu.Unlock()
	e.fetcher.answers[netip.MustParseAddr(ip)] = names
}

func (e *env) down(ip string) {
	e.fetcher.mu.Lock()
	defer e.fetcher.mu.Unlock()
	delete(e.fetcher.answers, netip.MustParseAddr(ip))
}

func (e *env) round(h *Hub) {
	e.t.Helper()
	if err := h.Round(context.Background()); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) records() []Record {
	e.t.Helper()
	b, err := os.ReadFile(e.cfg.OutputPath)
	if err != nil {
		e.t.Fatal(err)
	}
	var recs []Record
	if err := json.Unmarshal(b, &recs); err != nil {
		e.t.Fatalf("records file is not valid JSON: %v\n%s", err, b)
	}
	return recs
}

func (e *env) names() []string {
	var out []string
	for _, r := range e.records() {
		if !slices.Contains(out, r.Name) {
			out = append(out, r.Name)
		}
	}
	return out
}

func TestRoundWritesRecordsForTaggedNodes(t *testing.T) {
	e := newEnv(t)
	e.peers.peers = []Peer{
		peer("n1", "lab", "100.64.0.1", "fd7a:115c:a1e0::1", "tag:server"),
		peer("n2", "laptop", "100.64.0.2", "", "tag:other"),
	}
	e.serve("100.64.0.1", "reader.example.com", "DB.example.com.")
	e.serve("100.64.0.2", "evil.example.com")

	e.round(e.hub())

	want := []Record{
		{Name: "db.example.com", Type: "A", Value: "100.64.0.1"},
		{Name: "db.example.com", Type: "AAAA", Value: "fd7a:115c:a1e0::1"},
		{Name: "reader.example.com", Type: "A", Value: "100.64.0.1"},
		{Name: "reader.example.com", Type: "AAAA", Value: "fd7a:115c:a1e0::1"},
	}
	if got := e.records(); !slices.Equal(got, want) {
		t.Fatalf("records = %+v\nwant %+v", got, want)
	}
}

func TestEmptyTailnetWritesEmptyArray(t *testing.T) {
	e := newEnv(t)
	e.round(e.hub())

	b, err := os.ReadFile(e.cfg.OutputPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "[]\n" {
		t.Fatalf("records file = %q, want %q", b, "[]\n")
	}
}

func TestRejectsDisallowedNames(t *testing.T) {
	e := newEnv(t)
	e.peers.peers = []Peer{peer("n1", "lab", "100.64.0.1", "", "tag:server")}
	e.serve("100.64.0.1",
		"ok.example.com",
		"example.com",          // apex
		"hs.example.com",       // reserved
		"lab.ts.example.com",   // MagicDNS base domain
		"ts.example.com",       // the excluded domain itself
		"*.example.com",        // wildcard
		"bad_name.example.com", // invalid character
		"-dash.example.com",    // invalid label
		"other.org",            // wrong domain
		"notexample.com",       // suffix but not a subdomain
	)

	e.round(e.hub())

	if got := e.names(); !slices.Equal(got, []string{"ok.example.com"}) {
		t.Fatalf("names = %v, want [ok.example.com]", got)
	}
}

func TestConflictsResolvedByHostName(t *testing.T) {
	e := newEnv(t)
	e.peers.peers = []Peer{
		peer("n2", "zeta", "100.64.0.2", "", "tag:server"),
		peer("n1", "alpha", "100.64.0.1", "", "tag:server"),
	}
	e.serve("100.64.0.1", "shared.example.com", "a.example.com")
	e.serve("100.64.0.2", "shared.example.com", "z.example.com")

	e.round(e.hub())

	want := []Record{
		{Name: "a.example.com", Type: "A", Value: "100.64.0.1"},
		{Name: "shared.example.com", Type: "A", Value: "100.64.0.1"},
		{Name: "z.example.com", Type: "A", Value: "100.64.0.2"},
	}
	if got := e.records(); !slices.Equal(got, want) {
		t.Fatalf("records = %+v\nwant %+v", got, want)
	}
}

func TestUnreachableNodeKeptUntilGraceExpires(t *testing.T) {
	e := newEnv(t)
	e.peers.peers = []Peer{peer("n1", "lab", "100.64.0.1", "", "tag:server")}
	e.serve("100.64.0.1", "reader.example.com")
	h := e.hub()
	e.round(h)

	e.down("100.64.0.1")
	e.clock = e.clock.Add(9 * time.Minute)
	e.round(h)
	if got := e.names(); !slices.Equal(got, []string{"reader.example.com"}) {
		t.Fatalf("within grace: names = %v", got)
	}

	e.clock = e.clock.Add(2 * time.Minute)
	e.round(h)
	if got := e.names(); len(got) != 0 {
		t.Fatalf("after grace: names = %v, want none", got)
	}
}

func TestNodeLeavingTailnetKeptUntilGraceExpires(t *testing.T) {
	e := newEnv(t)
	e.peers.peers = []Peer{peer("n1", "lab", "100.64.0.1", "", "tag:server")}
	e.serve("100.64.0.1", "reader.example.com")
	h := e.hub()
	e.round(h)

	e.peers.peers = nil
	e.round(h)
	if got := e.names(); !slices.Equal(got, []string{"reader.example.com"}) {
		t.Fatalf("within grace: names = %v", got)
	}

	e.clock = e.clock.Add(11 * time.Minute)
	e.round(h)
	if got := e.names(); len(got) != 0 {
		t.Fatalf("after grace: names = %v, want none", got)
	}
}

func TestNodeLosingTagDroppedImmediately(t *testing.T) {
	e := newEnv(t)
	e.peers.peers = []Peer{peer("n1", "lab", "100.64.0.1", "", "tag:server")}
	e.serve("100.64.0.1", "reader.example.com")
	h := e.hub()
	e.round(h)

	e.peers.peers = []Peer{peer("n1", "lab", "100.64.0.1", "")}
	e.round(h)
	if got := e.names(); len(got) != 0 {
		t.Fatalf("names = %v, want none", got)
	}
}

func TestPeerListFailureChangesNothing(t *testing.T) {
	e := newEnv(t)
	e.peers.peers = []Peer{peer("n1", "lab", "100.64.0.1", "", "tag:server")}
	e.serve("100.64.0.1", "reader.example.com")
	h := e.hub()
	e.round(h)

	// tailscaled is down for longer than the grace period.
	e.peers.err = errors.New("socket gone")
	e.clock = e.clock.Add(time.Hour)
	if err := h.Round(context.Background()); err == nil {
		t.Fatal("expected error")
	}
	if got := e.names(); !slices.Equal(got, []string{"reader.example.com"}) {
		t.Fatalf("names = %v", got)
	}
}

func TestManifestUpdatesReplaceNames(t *testing.T) {
	e := newEnv(t)
	e.peers.peers = []Peer{peer("n1", "lab", "100.64.0.1", "", "tag:server")}
	e.serve("100.64.0.1", "a.example.com", "b.example.com")
	h := e.hub()
	e.round(h)

	e.serve("100.64.0.1", "b.example.com")
	e.round(h)
	if got := e.names(); !slices.Equal(got, []string{"b.example.com"}) {
		t.Fatalf("names = %v", got)
	}
}

func TestStateSurvivesRestart(t *testing.T) {
	e := newEnv(t)
	e.peers.peers = []Peer{peer("n1", "lab", "100.64.0.1", "", "tag:server")}
	e.serve("100.64.0.1", "reader.example.com")
	e.round(e.hub())

	// Restart while the node is unreachable: its names must survive.
	e.down("100.64.0.1")
	e.clock = e.clock.Add(time.Minute)
	e.round(e.hub())
	if got := e.names(); !slices.Equal(got, []string{"reader.example.com"}) {
		t.Fatalf("names = %v", got)
	}
}

func TestRecordsFileRepairedAndNotRewrittenNeedlessly(t *testing.T) {
	e := newEnv(t)
	e.peers.peers = []Peer{peer("n1", "lab", "100.64.0.1", "", "tag:server")}
	e.serve("100.64.0.1", "reader.example.com")
	h := e.hub()
	e.round(h)

	before, err := os.Stat(e.cfg.OutputPath)
	if err != nil {
		t.Fatal(err)
	}
	e.round(h)
	after, err := os.Stat(e.cfg.OutputPath)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("unchanged records were rewritten")
	}

	if err := os.Remove(e.cfg.OutputPath); err != nil {
		t.Fatal(err)
	}
	e.round(h)
	if got := e.names(); !slices.Equal(got, []string{"reader.example.com"}) {
		t.Fatalf("names after repair = %v", got)
	}
}

func TestNoTempFilesLeftBehind(t *testing.T) {
	e := newEnv(t)
	e.peers.peers = []Peer{peer("n1", "lab", "100.64.0.1", "", "tag:server")}
	e.serve("100.64.0.1", "reader.example.com")
	e.round(e.hub())

	entries, err := os.ReadDir(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, ent := range entries {
		names = append(names, ent.Name())
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"records.json", "state.json"}) {
		t.Fatalf("directory contains %v", names)
	}
}

func TestFetchUsesIPv4(t *testing.T) {
	got, ok := fetchAddr([]netip.Addr{netip.MustParseAddr("fd7a:115c:a1e0::1"), netip.MustParseAddr("100.64.0.1")})
	if !ok || got != netip.MustParseAddr("100.64.0.1") {
		t.Fatalf("fetchAddr = %v, %v", got, ok)
	}
	got, ok = fetchAddr([]netip.Addr{netip.MustParseAddr("fd7a:115c:a1e0::1")})
	if !ok || !got.Is6() {
		t.Fatalf("fetchAddr IPv6 only = %v, %v", got, ok)
	}
	if _, ok := fetchAddr(nil); ok {
		t.Fatal("fetchAddr(nil) succeeded")
	}
}
