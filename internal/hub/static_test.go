package hub

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func (e *env) writeStatic(content string) {
	e.t.Helper()
	if err := os.WriteFile(e.cfg.StaticRecordsPath, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) refresh(h *Hub) {
	e.t.Helper()
	if err := h.RefreshStatic(); err != nil {
		e.t.Fatal(err)
	}
}

func newStaticEnv(t *testing.T) *env {
	e := newEnv(t)
	e.cfg.StaticRecordsPath = filepath.Join(e.dir, "static.json")
	return e
}

func TestStaticRecordsMergedIntoOutput(t *testing.T) {
	e := newStaticEnv(t)
	e.peers.peers = []Peer{peer("n1", "lab", "100.64.0.1", "", "tag:server")}
	e.serve("100.64.0.1", "reader.example.com")
	// Static records are not limited to the configured domains.
	e.writeStatic(`[{"name": "NAS.home.arpa.", "type": "a", "value": "192.168.1.10"}]`)

	e.round(e.hub())

	want := []Record{
		{Name: "nas.home.arpa", Type: "A", Value: "192.168.1.10"},
		{Name: "reader.example.com", Type: "A", Value: "100.64.0.1"},
	}
	if got := e.records(); !slices.Equal(got, want) {
		t.Fatalf("records = %+v\nwant %+v", got, want)
	}
}

func TestStaticRecordsWinConflicts(t *testing.T) {
	e := newStaticEnv(t)
	e.peers.peers = []Peer{peer("n1", "lab", "100.64.0.1", "fd7a:115c:a1e0::1", "tag:server")}
	e.serve("100.64.0.1", "reader.example.com", "db.example.com")
	e.writeStatic(`[{"name": "db.example.com", "type": "A", "value": "100.64.0.9"}]`)

	e.round(e.hub())

	want := []Record{
		{Name: "db.example.com", Type: "A", Value: "100.64.0.9"},
		{Name: "reader.example.com", Type: "A", Value: "100.64.0.1"},
		{Name: "reader.example.com", Type: "AAAA", Value: "fd7a:115c:a1e0::1"},
	}
	if got := e.records(); !slices.Equal(got, want) {
		t.Fatalf("records = %+v\nwant %+v", got, want)
	}
	if !strings.Contains(e.logs.String(), "owner=\"static records\"") {
		t.Fatalf("conflict not logged:\n%s", e.logs.String())
	}
}

func TestMissingStaticFileHoldsNoRecords(t *testing.T) {
	e := newStaticEnv(t)
	e.peers.peers = []Peer{peer("n1", "lab", "100.64.0.1", "", "tag:server")}
	e.serve("100.64.0.1", "reader.example.com")

	e.round(e.hub())

	if got := e.names(); !slices.Equal(got, []string{"reader.example.com"}) {
		t.Fatalf("names = %v", got)
	}
}

func TestInvalidStaticRecordsSkipped(t *testing.T) {
	e := newStaticEnv(t)
	e.writeStatic(`[
		{"name": "ok.example.com", "type": "A", "value": "100.64.0.9"},
		{"name": "ok.example.com", "type": "A", "value": "100.64.0.9"},
		{"name": "*.example.com", "type": "A", "value": "100.64.0.9"},
		{"name": "cname.example.com", "type": "CNAME", "value": "ok.example.com"},
		{"name": "bad.example.com", "type": "A", "value": "not-an-ip"},
		{"name": "v6.example.com", "type": "A", "value": "fd7a::1"},
		{"name": "v4.example.com", "type": "AAAA", "value": "100.64.0.9"}
	]`)

	e.round(e.hub())

	if got := e.names(); !slices.Equal(got, []string{"ok.example.com"}) {
		t.Fatalf("names = %v", got)
	}
	if n := strings.Count(e.logs.String(), "rejecting static record"); n != 5 {
		t.Fatalf("logged %d rejections, want 5:\n%s", n, e.logs.String())
	}
}

func TestRefreshStaticPicksUpChangesWithoutFetching(t *testing.T) {
	e := newStaticEnv(t)
	e.peers.peers = []Peer{peer("n1", "lab", "100.64.0.1", "", "tag:server")}
	e.serve("100.64.0.1", "reader.example.com")
	h := e.hub()
	e.round(h)

	// The node goes down, but a refresh must neither fetch nor drop it.
	e.down("100.64.0.1")
	e.writeStatic(`[{"name": "nas.example.com", "type": "A", "value": "100.64.0.9"}]`)
	e.refresh(h)
	if got := e.names(); !slices.Equal(got, []string{"nas.example.com", "reader.example.com"}) {
		t.Fatalf("names after adding = %v", got)
	}
	if strings.Contains(e.logs.String(), "fetching manifest failed") {
		t.Fatal("refresh fetched manifests")
	}

	before, err := os.Stat(e.cfg.OutputPath)
	if err != nil {
		t.Fatal(err)
	}
	e.refresh(h)
	after, err := os.Stat(e.cfg.OutputPath)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("unchanged static records caused a rewrite")
	}

	e.writeStatic(`[]`)
	e.refresh(h)
	if got := e.names(); !slices.Equal(got, []string{"reader.example.com"}) {
		t.Fatalf("names after removing = %v", got)
	}
}

func TestUnparsableStaticFileKeepsLastGood(t *testing.T) {
	e := newStaticEnv(t)
	e.writeStatic(`[{"name": "nas.example.com", "type": "A", "value": "100.64.0.9"}]`)
	h := e.hub()
	e.round(h)

	// A writer caught halfway through.
	e.writeStatic(`[{"name": "nas.exa`)
	e.refresh(h)
	e.round(h)
	if got := e.names(); !slices.Equal(got, []string{"nas.example.com"}) {
		t.Fatalf("names with partial file = %v", got)
	}
	if n := strings.Count(e.logs.String(), "ignoring unparsable static records"); n != 1 {
		t.Fatalf("logged %d parse failures, want 1:\n%s", n, e.logs.String())
	}

	e.writeStatic(`[{"name": "nas2.example.com", "type": "A", "value": "100.64.0.9"}]`)
	e.refresh(h)
	if got := e.names(); !slices.Equal(got, []string{"nas2.example.com"}) {
		t.Fatalf("names after fix = %v", got)
	}
}
