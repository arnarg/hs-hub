package hub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"testing"
	"time"
)

func testServer(t *testing.T, h http.HandlerFunc) (*HTTPFetcher, netip.Addr) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	ap := netip.MustParseAddrPort(srv.Listener.Addr().String())
	return &HTTPFetcher{Port: ap.Port(), Path: "/services.json", Timeout: time.Second}, ap.Addr()
}

func TestHTTPFetcher(t *testing.T) {
	f, addr := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/services.json" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"services": ["reader.example.com", "db.example.com"]}`))
	})

	got, err := f.Fetch(context.Background(), addr)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"reader.example.com", "db.example.com"}) {
		t.Fatalf("got %v", got)
	}
}

func TestHTTPFetcherEmptyManifest(t *testing.T) {
	f, addr := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	got, err := f.Fetch(context.Background(), addr)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("got %#v, want empty non-nil slice", got)
	}
}

func TestHTTPFetcherErrors(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"not found": func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) },
		"bad json":  func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`<html>`)) },
		"wrong type": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"services": "reader.example.com"}`))
		},
		"timeout": func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, addr := testServer(t, h)
			f.Timeout = 200 * time.Millisecond
			if _, err := f.Fetch(context.Background(), addr); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}
