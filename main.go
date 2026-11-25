// Command hs-hub publishes services announced by tailnet nodes as Headscale
// extra DNS records.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/arnarg/hs-hub/internal/hub"
	"github.com/arnarg/hs-hub/internal/localapi"
)

// listFlag collects a flag that can be repeated and/or comma-separated.
type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }

func (l *listFlag) Set(v string) error {
	for s := range strings.SplitSeq(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			*l = append(*l, s)
		}
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "hs-hub:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		domains, exclude, reserved listFlag

		socket       = flag.String("socket", localapi.DefaultSocket, "tailscaled LocalAPI socket")
		tag          = flag.String("tag", "tag:server", "tag nodes must carry to publish services")
		port         = flag.Uint("manifest-port", 7777, "port nodes serve their manifest on")
		path         = flag.String("manifest-path", "/services.json", "HTTP path of the manifest")
		output       = flag.String("output", "", "records file read by Headscale (dns.extra_records_path)")
		state        = flag.String("state", "", "optional file persisting known nodes across restarts")
		interval     = flag.Duration("interval", 30*time.Second, "time between rounds")
		fetchTimeout = flag.Duration("fetch-timeout", 3*time.Second, "timeout for a single manifest fetch")
		grace        = flag.Duration("grace", 10*time.Minute, "how long to keep names of unreachable nodes")
		concurrency  = flag.Int("concurrency", 16, "maximum parallel manifest fetches")
		once         = flag.Bool("once", false, "run a single round and exit")
		logLevel     = flag.String("log-level", "info", "log level: debug, info, warn, error")
	)
	flag.Var(&domains, "domain", "domain published names must be below (repeatable)")
	flag.Var(&exclude, "exclude-domain", "domain whose names are never published, e.g. the MagicDNS base domain (repeatable)")
	flag.Var(&reserved, "reserved", "name that is never published, e.g. public names (repeatable)")
	flag.Parse()

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		return fmt.Errorf("invalid -log-level: %w", err)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	if *output == "" {
		return fmt.Errorf("-output is required")
	}
	if len(domains) == 0 {
		return fmt.Errorf("at least one -domain is required")
	}
	if *port == 0 || *port > 65535 {
		return fmt.Errorf("invalid -manifest-port %d", *port)
	}
	if *interval <= 0 {
		return fmt.Errorf("-interval must be positive")
	}

	h, err := hub.New(hub.Config{
		Tag:            *tag,
		Domains:        domains,
		ExcludeDomains: exclude,
		Reserved:       reserved,
		Grace:          *grace,
		Concurrency:    *concurrency,
		OutputPath:     *output,
		StatePath:      *state,
	}, localapi.New(*socket), &hub.HTTPFetcher{
		Port:    uint16(*port),
		Path:    *path,
		Timeout: *fetchTimeout,
		Client:  &http.Client{Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}},
	}, log)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *once {
		return h.Round(ctx)
	}

	// SIGHUP triggers an immediate round.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)

	log.Info("starting", "tag", *tag, "domains", domains.String(), "interval", *interval, "output", *output)

	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for {
		if err := h.Round(ctx); err != nil && ctx.Err() == nil {
			log.Error("round failed", "err", err)
		}
		select {
		case <-ctx.Done():
			log.Info("shutting down")
			return nil
		case <-ticker.C:
		case <-hup:
			log.Info("SIGHUP received, running round")
		}
	}
}
